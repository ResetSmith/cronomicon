package execspec

import (
	"context"
	"database/sql"
	"strings"
)

// Scope↔runner bindings (the scope-bound-runners plan, SB band; mig. 1180).
//
// A scope may name the runners allowed to serve it. A scope with NO binding
// row is unrestricted and dispatches exactly as it always has; a scope with at
// least one is claimable only by a runner named there, on top of the agency,
// capability and injection rules — the binding narrows and never widens.
//
// The rule is evaluated at CLAIM time against the live rows, not snapshotted
// onto the run at enqueue the way agencies are: a queued run must follow a
// runner swap, or replacing a runner strands everything already waiting for it.
//
// The binding outlives its runner. scope_runners.runner_id has no foreign key,
// so a deregistered runner leaves its row behind and the scope stays closed —
// which is why every reader here LEFT JOINs runners and reports `Registered`.

// BoundRunner is one runner a scope is bound to, as the Scopes view and the
// unclaimable explainer need it.
type BoundRunner struct {
	RunnerID string `json:"runnerId"`
	// Name is the runner's current name while it is registered, and the name
	// snapshotted at bind time once it is not.
	Name string `json:"name"`
	// Registered is false for a binding whose runner row is gone. Such a
	// binding still restricts the scope; nothing can satisfy it until an
	// operator replaces it or the runner's placement is restored.
	Registered bool `json:"registered"`
	// Status is the runner's status, "" when it is not registered.
	Status string `json:"status"`
	// Eligible reports whether the runner passes claimRun's agency rule for
	// this scope today: a member of one of the scope's agencies, or — for a
	// scope with none — a runner with no agencies. Checked at bind time and
	// allowed to drift afterwards, so it is reported rather than assumed.
	Eligible bool `json:"eligible"`
}

// rowQueryer is the read seam shared by *sql.DB and *sql.Tx.
type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// boundRunnersSelect lists a scope's bindings with the live runner beside each.
// The eligibility test is claimRun's agency rule, restated for one scope and
// one runner so the two cannot disagree about who may serve: the runner serves
// one of the scope's agencies. (One arm since migration 1220: a scope with "no
// agency" is Global's and a runner with none serves Global.)
const boundRunnersSelect = `
	SELECT sr.runner_id,
	       COALESCE(rn.name, sr.runner_name),
	       rn.id IS NOT NULL,
	       COALESCE(rn.status, ''),
	       CASE
	         WHEN rn.id IS NULL THEN 0
	         ELSE EXISTS (SELECT 1 FROM scope_agencies sa
	                        JOIN runner_agencies ra ON ra.agency_id = sa.agency_id
	                       WHERE sa.scope_id = sr.scope_id AND ra.runner_id = sr.runner_id)
	       END
	  FROM scope_runners sr
	  LEFT JOIN runners rn ON rn.id = sr.runner_id`

func scanBoundRunners(rows *sql.Rows) ([]BoundRunner, error) {
	defer rows.Close()
	out := []BoundRunner{}
	for rows.Next() {
		var b BoundRunner
		if err := rows.Scan(&b.RunnerID, &b.Name, &b.Registered, &b.Status, &b.Eligible); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BoundRunners returns the runners a scope (by id) is bound to, registered ones
// first, then by name. Empty (non-nil) for an unrestricted scope.
func BoundRunners(ctx context.Context, q rowQueryer, scopeID string) ([]BoundRunner, error) {
	rows, err := q.QueryContext(ctx, boundRunnersSelect+`
		 WHERE sr.scope_id = ?
		 ORDER BY (rn.id IS NULL), COALESCE(rn.name, sr.runner_name), sr.runner_id`, scopeID)
	if err != nil {
		return nil, err
	}
	return scanBoundRunners(rows)
}

// BoundRunnersByScopeName is BoundRunners for callers holding the scope NAME a
// run or a job carries. An empty name, or one with no scopes row, has no
// binding by construction.
func BoundRunnersByScopeName(ctx context.Context, q rowQueryer, scope string) ([]BoundRunner, error) {
	if strings.TrimSpace(scope) == "" {
		return []BoundRunner{}, nil
	}
	rows, err := q.QueryContext(ctx, boundRunnersSelect+`
		  JOIN scopes sc ON sc.id = sr.scope_id
		 WHERE sc.name = ?
		 ORDER BY (rn.id IS NULL), COALESCE(rn.name, sr.runner_name), sr.runner_id`, scope)
	if err != nil {
		return nil, err
	}
	return scanBoundRunners(rows)
}

// ScopeBindingPermits reports whether a runner may serve a scope (by name) as
// far as the binding is concerned: true for an unrestricted scope, and for a
// restricted one only when this runner is named. It says nothing about agency,
// capability or injection — callers apply those rules separately, as claimRun
// does.
func ScopeBindingPermits(ctx context.Context, q rowQueryer, scope, runnerID string) (bool, error) {
	if strings.TrimSpace(scope) == "" {
		return true, nil
	}
	var ok bool
	err := q.QueryRowContext(ctx, `
		SELECT NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                    WHERE sc.name = ?)
		    OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                WHERE sc.name = ? AND sr.runner_id = ?)`,
		scope, scope, runnerID).Scan(&ok)
	return ok, err
}

// scopeBindingReason explains why a run on a restricted scope has no claimable
// runner, given that an agency-eligible online runner of the right type exists
// somewhere. Three situations produce that symptom and each sends the operator
// somewhere different, so they are told apart rather than merged:
//
//   - every bound runner is deregistered → restore its placement or bind another;
//   - bound runners exist but none passes the agency rule → the binding has
//     drifted from the scope's agency; fix one or the other;
//   - otherwise they are offline, draining, or cannot run this type → go look at
//     those runners.
func scopeBindingReason(scope string, bound []BoundRunner) string {
	names := make([]string, 0, len(bound))
	registered, eligible := 0, 0
	for _, b := range bound {
		names = append(names, b.Name)
		if b.Registered {
			registered++
		}
		if b.Eligible {
			eligible++
		}
	}
	who := strings.Join(names, ", ")
	prefix := "scope " + scope + " is bound to " + who + ", and no bound runner is "
	switch {
	case registered == 0:
		return prefix + "registered any more — restore its placement or bind another runner"
	case eligible == 0:
		return prefix + "in the scope's agency"
	default:
		return prefix + "online and otherwise eligible"
	}
}

// RunnerEligibleForScope reports whether a REGISTERED runner passes claimRun's
// agency rule for a scope (by id): it serves one of the scope's agencies (Global
// among them, for a scope that is Global's). False for a runner that is not
// registered. This is the bind-time check: binding a runner the claim query
// would refuse produces a scope nothing can serve, which is better refused at
// the form than discovered as a stuck run.
//
// False for the local runner until it claims by the runner rule (Phase B): see
// ClaimsByPollSQL.
func RunnerEligibleForScope(ctx context.Context, q rowQueryer, scopeID, runnerID string) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM runners rn WHERE rn.id = ? AND `+ClaimsByPollSQL("rn")+`)
		   AND EXISTS (SELECT 1 FROM scope_agencies sa
		                 JOIN runner_agencies ra ON ra.agency_id = sa.agency_id
		                WHERE sa.scope_id = ? AND ra.runner_id = ?)`,
		runnerID, scopeID, runnerID).Scan(&ok)
	return ok, err
}

// ScopeBindingBlock returns the binding sentence when the scope binding is the
// gate holding a queued run, and "" when the run is claimable or is held by
// something else (no runner online, type, agency, pin, injection, tokens).
//
// It exists for the read-time stuck-run hint, which keeps its own wording for
// the gates it already explained and needs to know only whether THIS one is the
// cause. It answers by asking UnclaimableReason — the single place the gate
// order is decided — and recognising the one sentence only the binding gate can
// produce, rather than restating that order here where it could drift.
func ScopeBindingBlock(ctx context.Context, database *sql.DB, runID string) (string, error) {
	reason, err := UnclaimableReason(ctx, database, runID)
	if err != nil || reason == "" {
		return "", err
	}
	var scope sql.NullString
	if err := database.QueryRowContext(ctx, `SELECT scope FROM runs WHERE id = ?`, runID).Scan(&scope); err != nil {
		return "", nil //nolint:nilerr // a run that vanished mid-read has no hint to give
	}
	bound, err := BoundRunnersByScopeName(ctx, database, scope.String)
	if err != nil || len(bound) == 0 {
		return "", err
	}
	if reason == scopeBindingReason(scope.String, bound) {
		return reason, nil
	}
	return "", nil
}
