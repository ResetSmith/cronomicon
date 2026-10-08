package execspec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"strings"
)

// RA-20(b) — name the missing runner property at ENQUEUE.
//
// Every claim gate that saves correctness presents to an operator as the same
// symptom: the run sits queued and nothing happens. With a fleet of one that was
// survivable, because there was only one runner to inspect. As the fleet grows —
// and it is growing — "queued" stops being a diagnosis and becomes a mystery, and
// under the ops model (§13) the person who must diagnose it is the one furthest
// from the fleet: the agency user only knows their job did not run.
//
// Computed at enqueue and STORED rather than derived on read, because the question
// is usually asked in the past tense. "Why didn't it run last night?" cannot be
// answered by a probe of today's fleet — the runner that was missing may be online
// now. A stored reason survives into run history with the run it explains.
//
// The reason is advisory: it never blocks the enqueue and never affects claiming.
// A run whose runner appears a minute later is claimed normally, and the stale
// reason is cleared by the claim.

// UnclaimableReason returns a human sentence naming WHY no online runner can claim
// the run, or "" when one can. It narrows through claimRun's gates in the order an
// operator would check them, so the sentence names the FIRST thing that is wrong
// rather than the last — "no runner is online" is more useful than "no runner
// advertises become-file" when both are true.
//
// Fleet-capability information is deliberately included. The audience holds
// triggerJobs on the run's scope, the facts are about infrastructure rather than
// credentials, and a reason vague enough to leak nothing is a reason nobody can act
// on — which is the entire failure being fixed.
func UnclaimableReason(ctx context.Context, database *sql.DB, runID string) (string, error) {
	var agenciesJSON, runType, requiresJSON, scope, executor sql.NullString
	err := database.QueryRowContext(ctx, `
		SELECT COALESCE(agencies_json,'[]'), run_type, requires_json, scope, executor
		FROM runs WHERE id = ?`, runID).
		Scan(&agenciesJSON, &runType, &requiresJSON, &scope, &executor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Nothing claims a row frozen onto the SSH executor any more (2.3.0, LR-42):
	// every run is written for the runner executor and the server takes its
	// share as the local runner. The upgrade converts the rows that were
	// waiting; one that is still here was written around the one writer of runs.
	if executor.String == "ssh" {
		return ReasonQueuedForSSHExecutor, nil
	}
	ok, _, err := EligibleOnlineRunnerForRun(ctx, database, runID)
	if err != nil || ok {
		return "", err
	}
	ag := agenciesJSON.String
	if ag == "" || ag == "null" {
		ag = "[]"
	}
	bindsKey, err := runBindsKey(ctx, database, runID)
	if err != nil {
		return "", err
	}

	// 0. The local runner would take this run and is not running. Asked first,
	// because it is the one cause with a single switch behind it, and because
	// every line below counts ONLINE runners and would say "no runner is
	// online" about a server that is merely turned off. Type, requirements,
	// agency, binding and the key rule, as the claim applies them; secret
	// injection is always on for it (LR-46).
	reqJSON := requiresJSON.String
	if reqJSON == "" {
		reqJSON = "[]"
	}
	lcaps := effectiveCapsSQL("rn")
	var localOff bool
	if err := database.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM runners rn
			 WHERE rn.kind = 'server' AND rn.status <> 'online'
			   AND ? IN `+lcaps+`
			   AND NOT EXISTS (SELECT 1 FROM json_each(?) je WHERE je.value NOT IN `+lcaps+`)
			   AND EXISTS (SELECT 1 FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
			                WHERE ra.runner_id = rn.id AND a.name IN (SELECT value FROM json_each(?)))
			   AND (NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id WHERE sc.name = ?)
			        OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                    WHERE sc.name = ? AND sr.runner_id = rn.id))
			   AND ? = 0)`,
		runType.String, reqJSON, ag, scope.String, scope.String, bindsKey).Scan(&localOff); err != nil {
		return "", err
	}
	if localOff {
		return ReasonLocalRunnerOff, nil
	}

	count := func(where string, args ...any) (int, error) {
		var n int
		e := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM runners rn WHERE rn.status = 'online' AND `+where, args...).Scan(&n)
		return n, e
	}

	// 1. Is anything online at all?
	n, err := count(`1 = 1`)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "no runner is online", nil
	}

	// 2. Can anything online run this TYPE? Against the runner's EFFECTIVE
	// capabilities — what it declared minus what the operator masked off — as the
	// claim does: a runner whose mask lists this type will never take the run.
	caps := effectiveCapsSQL("rn")
	typeClause := `? IN ` + caps
	n, err = count(typeClause, runType.String)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "no online runner can run " + runType.String + " jobs", nil
	}

	// 3. Agency eligibility — claimRun's disjoint split (AG-Q3a), reproduced exactly.
	// A run needs a runner that SERVES one of its agencies. A run with no scope
	// is Global's (migration 1220) and needs a runner that serves Global — the
	// half that surprises people: it is not claimable by "anything", so a fleet in
	// which every runner belongs to a department cannot take it at all.
	agencyClause := typeClause + ` AND EXISTS (
		SELECT 1 FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
		 WHERE ra.runner_id = rn.id AND a.name IN (SELECT value FROM json_each(?)))`
	n, err = count(agencyClause, runType.String, ag)
	if err != nil {
		return "", err
	}
	if n == 0 {
		var names []string
		_ = json.Unmarshal([]byte(ag), &names)
		switch {
		case len(names) == 0:
			// Not reachable through the one writer of runs or the birth trigger; a
			// row in this state was written around both.
			return "this run carries no agency at all, so no runner can claim it (an internal error: " +
				"every run belongs to Global or to its scope's agency)", nil
		case len(names) == 1 && names[0] == agencyid.GlobalName:
			return "this run belongs to Global (it has no scope, or its scope is Global's), and no online " +
				"runner serves Global — bind a scope whose agency has a runner, or enrol a Global runner", nil
		}
		return "no online runner serves " + strings.Join(names, ", "), nil
	}

	// 3.4. The SB-1 scope binding (mig. 1180). Checked right after agency because
	// it is the same question asked more narrowly — "which runners in this
	// department can reach these hosts" — and before injection, which only
	// matters among runners that can. An unrestricted scope passes on the NOT
	// EXISTS arm. The arguments are kept in a slice so every later clause extends one
	// list instead of re-spelling it.
	bindClause := agencyClause + ` AND (
		NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id WHERE sc.name = ?)
		OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		            WHERE sc.name = ? AND sr.runner_id = rn.id))`
	bindArgs := []any{runType.String, ag, scope.String, scope.String}
	n, err = count(bindClause, bindArgs...)
	if err != nil {
		return "", err
	}
	if n == 0 {
		bound, berr := BoundRunnersByScopeName(ctx, database, scope.String)
		if berr != nil {
			return "", berr
		}
		return scopeBindingReason(scope.String, bound), nil
	}

	// 3.6. An SSH key binding (LR-47). The runners that passed so far may be the
	// local runner alone, and it does not take a run that binds a key: an agent
	// is handed the key as a file on its own disk, the server would have to
	// place one on the target.
	keyClause := bindClause + ` AND ` + localRunnerCannotSQL("rn")
	keyArgs := append(append([]any{}, bindArgs...), bindsKey)
	n, err = count(keyClause, keyArgs...)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return ReasonKeyNeedsAgent, nil
	}

	// 4. Secret injection (P1.4): a run whose job/script declares bindings needs an
	// injection-flagged runner. This is the gate most likely to be hit by a NEW
	// runner — enrolling it in the agency is the obvious step, ticking the
	// injection flag is the one people forget.
	// The question is claimrule.go's, shared with the claim's other mirror: keyed
	// on the run's frozen job identity, counting a per-run SSH credential, and
	// disarmed with the injection kill switch.
	bindsSecrets, err := runNeedsInjectionRunner(ctx, database, runID)
	if err != nil {
		return "", err
	}
	injectionClause := keyClause + ` AND (? = 0 OR rn.allow_secret_injection = 1)`
	injectionArgs := append(append([]any{}, keyArgs...), bindsSecrets)
	n, err = count(injectionClause, injectionArgs...)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "this run carries credentials, and no eligible runner is flagged for secret injection", nil
	}

	// 5. Requirement tokens — whatever is left. Named individually, because
	// "requirements unmet" sends an operator to read JSON.
	var requires []string
	if requiresJSON.Valid && requiresJSON.String != "" {
		_ = json.Unmarshal([]byte(requiresJSON.String), &requires)
	}
	var missing []string
	for _, tok := range requires {
		n, err = count(injectionClause+` AND ? IN `+caps,
			append(append([]any{}, injectionArgs...), tok)...)
		if err != nil {
			return "", err
		}
		if n == 0 {
			missing = append(missing, tok)
		}
	}
	if len(missing) > 0 {
		return "no eligible runner advertises " + strings.Join(missing, ", "), nil
	}

	// Every gate above passed individually but the combined predicate did not — the
	// tokens are satisfiable only by DIFFERENT runners. Rare, and worth saying
	// plainly rather than guessing.
	return "no single online runner satisfies all of this run's requirements", nil
}

// The three reasons 2.3.0 added with the local runner. Sentences, shown
// verbatim (there is no token-to-text map on the frontend).
const (
	// ReasonLocalRunnerOff: the local runner serves this run and is not running.
	ReasonLocalRunnerOff = "the local runner would take this run and is not running — it is turned off, or " +
		"forbidden on this host (a global administrator: Settings → Local runner)"
	// ReasonKeyNeedsAgent: only the local runner is in reach, and the job binds a key.
	ReasonKeyNeedsAgent = "this job binds an SSH key, which the local runner cannot deliver, and no agent " +
		"that could take the run is online"
	// ReasonQueuedForSSHExecutor: a row no claim reads any more.
	ReasonQueuedForSSHExecutor = "this run was queued for the SSH executor, which the local runner replaced in " +
		"2.3.0, so nothing will claim it — cancel it and run the job again"
)

// StampUnclaimableReason writes UnclaimableReason onto a queued run, if any. Never
// fails the caller: an advisory hint that errors must not take an enqueue down with
// it, so the error is returned for logging and the run stands either way.
//
// Only ever writes to a run still in 'queued' — by the time this runs, a fast
// runner may already have claimed it, and overwriting a live run's reason column
// with a stale "nobody can claim this" would be worse than saying nothing.
func StampUnclaimableReason(ctx context.Context, database *sql.DB, runID string) (string, error) {
	reason, err := UnclaimableReason(ctx, database, runID)
	if err != nil || reason == "" {
		return "", err
	}
	_, err = database.ExecContext(ctx,
		`UPDATE runs SET queued_reason = ? WHERE id = ? AND status = 'queued'`, reason, runID)
	return reason, err
}
