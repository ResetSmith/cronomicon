package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/hostkeys"
)

// Scope↔runner bindings — the write side (the scope-bound-runners plan, SB band;
// mig. 1180). The dispatch side, and the rules a binding obeys, are documented
// in internal/execspec/scopebinding.go.
//
// A binding is an operator-owned overlay on a scope of either source, exactly
// like its agency: set here, never parsed from Git, untouched by sync.

// ErrRunnerNotEligible is returned when a binding would name a runner the claim
// query refuses for that scope — one outside the scope's agency, or an agency
// member on a general-pool scope. Mapped 422. Scopes lists the scope name(s)
// the refusal is about, for the caller's message.
type ErrRunnerNotEligible struct {
	Runner string
	Scopes []string
}

func (e *ErrRunnerNotEligible) Error() string {
	return fmt.Sprintf("runner %s is not eligible for scope %s", e.Runner, strings.Join(e.Scopes, ", "))
}

// ErrBindingsChanged is returned by SetScopeRunners when the set of runners the
// write would ADD is no longer the set the caller was authorized for — another
// operator changed the scope's bindings between the authorization and the
// write. Mapped 409: reload and retry.
var ErrBindingsChanged = errors.New("scope bindings changed")

// ErrBoundScopeBusy is returned when a rename or delete would strand work on a
// scope that is bound to runners. A run carries its scope by NAME; rename or
// delete the scope and no row answers to that name any more, so the run reads as
// unrestricted and any runner in its agency may claim it — the fail-open a
// binding exists to prevent, reached by editing the scope instead of losing the
// runner. Refused (409) until the work drains or the operator unbinds the scope
// on purpose.
type ErrBoundScopeBusy struct {
	Scope   string
	Queued  int // queued runner runs under this scope name
	Pending int // parked or deferred runs under this scope name
}

func (e *ErrBoundScopeBusy) Error() string {
	return fmt.Sprintf("scope %s is bound to runners and has %d queued and %d scheduled run(s) waiting under "+
		"this name; they would become claimable by any runner in the agency. Let them finish or cancel them, "+
		"or unbind the scope first", e.Scope, e.Queued, e.Pending)
}

// BoundScopeBusySQL is a predicate over a `scopes` row (aliased by the caller as
// the bare table name) that is true when the scope is bound to runners AND has
// work waiting under its name. Exported as SQL because git sync applies the same
// rule inside its prune DELETE, where it must be part of the statement.
const BoundScopeBusySQL = `(
	EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = scopes.id)
	AND (EXISTS (SELECT 1 FROM runs r
	              WHERE r.scope = scopes.name AND r.status = 'queued' AND r.executor = 'runner')
	     OR EXISTS (SELECT 1 FROM pending_runs p
	                 WHERE p.scope = scopes.name AND p.status = 'pending')))`

// boundScopeBusy returns a non-nil *ErrBoundScopeBusy when the scope may not be
// renamed or deleted right now, nil when it may.
func boundScopeBusy(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, scopeID, scopeName string) (*ErrBoundScopeBusy, error) {
	var bound, queued, pending int
	if err := q.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM scope_runners WHERE scope_id = ?),
		       (SELECT COUNT(*) FROM runs WHERE scope = ? AND status = 'queued' AND executor = 'runner'),
		       (SELECT COUNT(*) FROM pending_runs WHERE scope = ? AND status = 'pending')`,
		scopeID, scopeName, scopeName).Scan(&bound, &queued, &pending); err != nil {
		return nil, fmt.Errorf("check bound scope: %w", err)
	}
	if bound == 0 || queued+pending == 0 {
		return nil, nil
	}
	return &ErrBoundScopeBusy{Scope: scopeName, Queued: queued, Pending: pending}, nil
}

// ErrNoBindings is returned by ReplaceScopeRunner when the runner being replaced
// is bound to nothing. Mapped 409: the caller's view is stale.
var ErrNoBindings = errors.New("runner is not bound to any scope")

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// normalizeIDs trims, drops blanks and de-dupes, preserving first-seen order.
func normalizeIDs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// scopeRunnerIDs returns the runner ids a scope is bound to today.
func scopeRunnerIDs(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, scopeID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT runner_id FROM scope_runners WHERE scope_id = ?`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ScopeRunnersDelta reports which runner ids a full-replace of a scope's
// bindings would ADD and REMOVE. The API authorizes each added runner
// individually before the write (a denial names the runner); removal needs no
// per-runner authority — it is an edit to the scope's own overlay.
func ScopeRunnersDelta(ctx context.Context, database *sql.DB, scopeID string, runnerIDs []string) (added, removed []string, err error) {
	current, err := scopeRunnerIDs(ctx, database, scopeID)
	if err != nil {
		return nil, nil, err
	}
	want := map[string]bool{}
	for _, id := range normalizeIDs(runnerIDs) {
		want[id] = true
		if !current[id] {
			added = append(added, id)
		}
	}
	for id := range current {
		if !want[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return added, removed, nil
}

// SetScopeRunners replaces a scope's bound-runner set. An empty set clears the
// binding and returns the scope to unrestricted dispatch.
//
// A runner being ADDED must be registered (ErrUnknownRunner) and eligible for
// the scope's agency (ErrRunnerNotEligible). A binding already present is kept
// as it is, registered or not: the deregistered "ghost" rows are precisely the
// ones an operator must be able to leave in place while adding a replacement,
// and re-validating them would make every save of such a scope fail.
//
// mayAdd is asked about each runner the write would add, INSIDE the transaction
// that adds it; a false answer aborts with ErrBindingsChanged. It is how the API
// binds the per-runner authorization it performed to the write that follows, so
// the two cannot be separated by a concurrent edit. nil permits every addition
// (callers with no per-runner authority to carry).
//
// One transaction: a half-applied replace would leave a scope bound to nobody
// (open) or to the old and new sets at once.
func SetScopeRunners(ctx context.Context, database *sql.DB, scopeID string, runnerIDs []string, actor string,
	mayAdd func(runnerID string) bool) (*Scope, error) {
	sc, err := GetScope(ctx, database, scopeID)
	if err != nil || sc == nil {
		return nil, err
	}
	ids := normalizeIDs(runnerIDs)

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	current, err := scopeRunnerIDs(ctx, tx, scopeID)
	if err != nil {
		return nil, fmt.Errorf("set scope runners: %w", err)
	}
	want := make(map[string]bool, len(ids))
	now := nowRFC3339()
	for _, id := range ids {
		want[id] = true
		if current[id] {
			continue
		}
		if mayAdd != nil && !mayAdd(id) {
			return nil, ErrBindingsChanged
		}
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrUnknownRunner
			}
			return nil, fmt.Errorf("set scope runners: %w", err)
		}
		ok, err := execspec.RunnerEligibleForScope(ctx, tx, scopeID, id)
		if err != nil {
			return nil, fmt.Errorf("set scope runners: %w", err)
		}
		if !ok {
			return nil, &ErrRunnerNotEligible{Runner: name, Scopes: []string{sc.Scope}}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
			VALUES (?, ?, ?, ?, ?)`, scopeID, id, name, actor, now); err != nil {
			return nil, fmt.Errorf("set scope runners: %w", err)
		}
	}
	for id := range current {
		if want[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM scope_runners WHERE scope_id = ? AND runner_id = ?`, scopeID, id); err != nil {
			return nil, fmt.Errorf("set scope runners: %w", err)
		}
	}
	bound, err := execspec.BoundRunners(ctx, tx, scopeID)
	if err != nil {
		return nil, fmt.Errorf("set scope runners: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	detail := "cleared (any eligible runner)"
	if len(bound) > 0 {
		names := make([]string, 0, len(bound))
		for _, b := range bound {
			names = append(names, b.Name)
		}
		detail = strings.Join(names, ", ")
	}
	audit(ctx, database, actor, "Scopes", "runners-set", sc.Scope, detail)
	return GetScope(ctx, database, scopeID)
}

// ReplaceScopeRunner swaps one runner for another on EVERY scope the first is
// bound to — the one-step form of "this host was replaced". The runner being
// replaced may be deregistered; that is the common case, and the reason this
// takes ids rather than requiring both rows to exist.
//
// All or nothing: the replacement must be eligible for every affected scope, and
// a refusal names the scopes it is not eligible for so the operator can fix its
// agency membership first. Returns the affected scope names, sorted.
func ReplaceScopeRunner(ctx context.Context, database *sql.DB, fromRunnerID, toRunnerID, actor string) ([]string, error) {
	fromRunnerID, toRunnerID = strings.TrimSpace(fromRunnerID), strings.TrimSpace(toRunnerID)
	if fromRunnerID == "" || toRunnerID == "" || fromRunnerID == toRunnerID {
		return nil, ErrUnknownRunner
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var toName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, toRunnerID).Scan(&toName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnknownRunner
		}
		return nil, fmt.Errorf("replace scope runner: %w", err)
	}

	type bound struct{ scopeID, scopeName, fromName string }
	var affected []bound
	rows, err := tx.QueryContext(ctx, `
		SELECT sr.scope_id, sc.name, sr.runner_name
		  FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		 WHERE sr.runner_id = ?
		 ORDER BY sc.name`, fromRunnerID)
	if err != nil {
		return nil, fmt.Errorf("replace scope runner: %w", err)
	}
	for rows.Next() {
		var b bound
		if err := rows.Scan(&b.scopeID, &b.scopeName, &b.fromName); err != nil {
			rows.Close()
			return nil, fmt.Errorf("replace scope runner: %w", err)
		}
		affected = append(affected, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replace scope runner: %w", err)
	}
	if len(affected) == 0 {
		return nil, ErrNoBindings
	}

	var refused []string
	for _, b := range affected {
		ok, err := execspec.RunnerEligibleForScope(ctx, tx, b.scopeID, toRunnerID)
		if err != nil {
			return nil, fmt.Errorf("replace scope runner: %w", err)
		}
		if !ok {
			refused = append(refused, b.scopeName)
		}
	}
	if len(refused) > 0 {
		return nil, &ErrRunnerNotEligible{Runner: toName, Scopes: refused}
	}

	// OR IGNORE, then sweep: a scope already bound to BOTH runners would collide
	// on the primary key, and for that scope the right outcome is simply that
	// the old row goes.
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `
		UPDATE OR IGNORE scope_runners
		   SET runner_id = ?, runner_name = ?, bound_by = ?, bound_at = ?
		 WHERE runner_id = ?`, toRunnerID, toName, actor, now, fromRunnerID); err != nil {
		return nil, fmt.Errorf("replace scope runner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scope_runners WHERE runner_id = ?`, fromRunnerID); err != nil {
		return nil, fmt.Errorf("replace scope runner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(affected))
	for _, b := range affected {
		names = append(names, b.scopeName)
	}
	audit(ctx, database, actor, "Scopes", "runner-replaced",
		affected[0].fromName+" → "+toName, strings.Join(names, ", "))
	return names, nil
}

// ── Retired runner pins (the migration's notice list) ──────────────────────

// RetiredPin is one runner-tag pin that could not be turned into a scope
// binding: by migration 1180 at upgrade, or by git sync for a job whose YAML
// still carries `runner_tag` on a scope with no binding.
type RetiredPin struct {
	ID         int64  `json:"id"`
	JobUID     string `json:"jobUid"`
	JobName    string `json:"jobName"`
	JobSource  string `json:"jobSource"`
	Scope      string `json:"scope"`
	RunnerTag  string `json:"runnerTag"`
	Reason     string `json:"reason"`
	RecordedAt string `json:"recordedAt"`
}

// ListRetiredPins returns the notices that still need an operator: not
// dismissed, and on a scope that has not since been bound. Binding the scope IS
// the resolution, so those rows drop out by themselves rather than waiting for
// someone to dismiss what they already fixed. A job with no scope has nothing to
// bind and stays until dismissed.
func ListRetiredPins(ctx context.Context, database *sql.DB) ([]RetiredPin, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT p.id, COALESCE(p.job_uid, ''), p.job_name, p.job_source, p.scope, p.runner_tag,
		       p.reason, p.recorded_at
		  FROM retired_runner_pins p
		 WHERE p.dismissed_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                    WHERE sc.name = p.scope)
		 ORDER BY p.scope, p.job_name, p.id`)
	if err != nil {
		return nil, fmt.Errorf("list retired runner pins: %w", err)
	}
	defer rows.Close()
	out := []RetiredPin{}
	for rows.Next() {
		var p RetiredPin
		if err := rows.Scan(&p.ID, &p.JobUID, &p.JobName, &p.JobSource, &p.Scope, &p.RunnerTag,
			&p.Reason, &p.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DismissRetiredPins marks notices as seen and deliberately left alone. Returns
// how many rows were newly dismissed; an id that is unknown or already dismissed
// is not an error (two operators may be looking at the same list).
func DismissRetiredPins(ctx context.Context, database *sql.DB, ids []int64, actor string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	now := nowRFC3339()
	n := 0
	for _, id := range ids {
		res, err := database.ExecContext(ctx, `
			UPDATE retired_runner_pins SET dismissed_at = ?, dismissed_by = ?
			 WHERE id = ? AND dismissed_at IS NULL`, now, actor, id)
		if err != nil {
			return n, fmt.Errorf("dismiss retired runner pin: %w", err)
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	if n > 0 {
		audit(ctx, database, actor, "Scopes", "runner-pin-notices-dismissed", fmt.Sprintf("%d notice(s)", n), "")
	}
	return n, nil
}

// ── Bind preview ────────────────────────────────────────────────────────────

// PreviewJob is one job on a scope, as far as a binding change concerns it.
type PreviewJob struct {
	UID     string `json:"uid"`
	Name    string `json:"name"`
	Source  string `json:"source"`
	RunType string `json:"runType"`
}

// PreviewRunner is one PROPOSED bound runner and what it can actually serve.
type PreviewRunner struct {
	RunnerID   string `json:"runnerId"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	Eligible   bool   `json:"eligible"`
	// Capabilities are the runner's EFFECTIVE capability tokens: what it
	// declared, minus the server-managed mask — the set the claim query uses.
	Capabilities []string `json:"capabilities"`
	// MissingRunTypes are run types the scope's jobs use that this runner cannot
	// run. Those jobs would wait for another bound runner, or forever.
	MissingRunTypes []string `json:"missingRunTypes"`
	// AllowsSecretInjection is the operator grant a run that binds secrets or an
	// SSH key needs; compare with ScopeRunnersPreview.JobsNeedingInjection.
	AllowsSecretInjection bool `json:"allowsSecretInjection"`
	// HostsWithoutKey is how many of the scope's hosts this runner does not
	// trust: neither approved here nor reported in its own known_hosts file.
	// A run on such a host fails at the first connection. Compare with
	// ScopeRunnersPreview.ScopeHosts.
	HostsWithoutKey int `json:"hostsWithoutKey"`
}

// ScopeRunnersPreview is what PUT /scopes/{id}/runners WOULD do, computed
// without doing it.
//
// Binding a scope is not only a dispatch change. A job with no executor of its
// own moves from the ssh executor (the control plane, with the server's
// credentials and known_hosts) to the bound runners (their own key directory or
// a delivered key, their own known_hosts); a job that asks for ssh starts being
// refused; and clearing a binding moves everything back. None of that is visible
// from the form, so the form asks first.
type ScopeRunnersPreview struct {
	Scope          string `json:"scope"`
	CurrentlyBound bool   `json:"currentlyBound"`
	WillBeBound    bool   `json:"willBeBound"`
	// JobsMovingToRunner resolve to ssh today and would resolve to the runner.
	JobsMovingToRunner []PreviewJob `json:"jobsMovingToRunner"`
	// JobsMovingToSSH resolve to the runner today only because the scope is
	// bound, and would return to ssh — to running from the server.
	JobsMovingToSSH []PreviewJob `json:"jobsMovingToSsh"`
	// JobsRefused ask for the ssh executor themselves and would be refused
	// (scope_requires_runner) while the scope is bound.
	JobsRefused []PreviewJob `json:"jobsRefused"`
	// QueuedSSHRuns are runs already queued on this scope, or parked for later,
	// and frozen onto ssh. They keep that executor and run from the server
	// whatever is saved.
	QueuedSSHRuns int `json:"queuedSshRuns"`
	// RunTypes are the run types the scope's jobs use; JobsNeedingInjection is
	// how many of them need a secret-injection runner.
	RunTypes             []string `json:"runTypes"`
	JobsNeedingInjection int      `json:"jobsNeedingInjection"`
	// ScopeHosts is how many hosts the scope has.
	ScopeHosts int             `json:"scopeHosts"`
	Runners    []PreviewRunner `json:"runners"`
}

// PreviewScopeRunners computes the effect of replacing a scope's bound-runner
// set with runnerIDs. It validates nothing and writes nothing: an unregistered
// or ineligible runner is reported as such, because showing why a save would be
// refused is part of the preview's job. Returns nil for an unknown scope.
func PreviewScopeRunners(ctx context.Context, database *sql.DB, scopeID string, runnerIDs []string) (*ScopeRunnersPreview, error) {
	sc, err := GetScope(ctx, database, scopeID)
	if err != nil || sc == nil {
		return nil, err
	}
	ids := normalizeIDs(runnerIDs)
	out := &ScopeRunnersPreview{
		Scope:              sc.Scope,
		CurrentlyBound:     len(sc.BoundRunners) > 0,
		WillBeBound:        len(ids) > 0,
		JobsMovingToRunner: []PreviewJob{},
		JobsMovingToSSH:    []PreviewJob{},
		JobsRefused:        []PreviewJob{},
		RunTypes:           []string{},
		Runners:            []PreviewRunner{},
	}

	// The scope's live jobs. Read fully before resolving anything: the resolver
	// queries too, and a cursor held open across it is the nested-cursor shape
	// this codebase avoids on SQLite.
	type jobRow struct {
		PreviewJob
		needsInjection bool
	}
	rows, err := database.QueryContext(ctx, `
		SELECT COALESCE(j.uid, ''), j.name, j.source, j.run_type,
		       (COALESCE(j.ssh_credential, '') <> ''
		        OR EXISTS (SELECT 1 FROM reference_bindings rb
		                    WHERE (rb.owner_kind = 'job'
		                            AND CASE WHEN COALESCE(j.uid, '') <> '' THEN rb.owner_uid = j.uid
		                                     ELSE rb.owner_source = j.source AND rb.owner_name = j.name END)
		                       OR (rb.owner_kind = 'script' AND rb.owner_name = j.script_ref)))
		  FROM jobs j
		 WHERE j.scope = ? AND j.deleted_at IS NULL
		 ORDER BY j.name, j.source`, sc.Scope)
	if err != nil {
		return nil, fmt.Errorf("preview scope runners: %w", err)
	}
	var jobs []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.UID, &j.Name, &j.Source, &j.RunType, &j.needsInjection); err != nil {
			rows.Close()
			return nil, fmt.Errorf("preview scope runners: %w", err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("preview scope runners: %w", err)
	}

	runTypes := map[string]bool{}
	for _, j := range jobs {
		eq := execspec.ExecutorQuery{
			JobUID: j.UID, JobSource: j.Source, JobName: j.Name, RunType: j.RunType, Scope: sc.Scope,
		}
		before := execspec.ResolveExecutorGiven(ctx, database, eq, out.CurrentlyBound)
		after := execspec.ResolveExecutorGiven(ctx, database, eq, out.WillBeBound)
		if before.Err != nil {
			return nil, fmt.Errorf("preview scope runners: %w", before.Err)
		}
		if after.Err != nil {
			return nil, fmt.Errorf("preview scope runners: %w", after.Err)
		}
		switch {
		case after.ScopeRefused():
			out.JobsRefused = append(out.JobsRefused, j.PreviewJob)
			continue // it will not run here at all, so it sets no requirement
		case before.Executor == execspec.ExecutorSSH && after.Executor == execspec.ExecutorRunner:
			out.JobsMovingToRunner = append(out.JobsMovingToRunner, j.PreviewJob)
		case after.Executor == execspec.ExecutorSSH && after.Refusal == nil &&
			(before.Executor == execspec.ExecutorRunner || before.ScopeRefused()):
			// Either it ran on the bound runners, or it asked for ssh and was being
			// refused. Clear the binding and both run from the server.
			out.JobsMovingToSSH = append(out.JobsMovingToSSH, j.PreviewJob)
		}
		if after.Executor == execspec.ExecutorRunner {
			runTypes[j.RunType] = true
			if j.needsInjection {
				out.JobsNeedingInjection++
			}
		}
	}
	for rt := range runTypes {
		out.RunTypes = append(out.RunTypes, rt)
	}
	sort.Strings(out.RunTypes)

	// Queued rows, plus runs parked for later (deferred, or held behind a Queue
	// gate): a parked run's executor was frozen when it was parked, so it is
	// promoted onto ssh exactly like a row already in the queue. json_valid
	// guards the extract — workflow rows carry no params, and one unreadable
	// blob must not fail the whole preview.
	if err := database.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM runs
		         WHERE scope = ? AND status = 'queued' AND executor = 'ssh')
		     + (SELECT COUNT(*) FROM pending_runs
		         WHERE scope = ? AND status = 'pending'
		           AND json_valid(params_json)
		           AND json_extract(params_json, '$.Executor') = 'ssh')`,
		sc.Scope, sc.Scope).Scan(&out.QueuedSSHRuns); err != nil {
		return nil, fmt.Errorf("preview scope runners: %w", err)
	}

	already := map[string]execspec.BoundRunner{}
	for _, b := range sc.BoundRunners {
		already[b.RunnerID] = b
	}
	// Moving a scope onto a runner moves the source of host-key trust with it:
	// the server's pins stop mattering and the runner's known_hosts starts to.
	scopeHosts, err := hostkeys.PlanScope(ctx, database, sc.Scope)
	if err != nil {
		return nil, fmt.Errorf("preview scope runners: %w", err)
	}
	out.ScopeHosts = len(scopeHosts)
	for _, id := range ids {
		pr := PreviewRunner{RunnerID: id, Name: id, Capabilities: []string{}, MissingRunTypes: []string{}}
		trusted, err := hostkeys.LoadTrusted(ctx, database, id)
		if err != nil {
			return nil, fmt.Errorf("preview scope runners: %w", err)
		}
		for _, h := range scopeHosts {
			if trusted.State(h) == hostkeys.StateNone {
				pr.HostsWithoutKey++
			}
		}
		var name, capsJSON string
		var mask sql.NullString
		var injection int
		err = database.QueryRowContext(ctx, `
			SELECT name, COALESCE(capabilities, '[]'),
			       CASE WHEN json_valid(managed_settings)
			            THEN json_extract(managed_settings, '$.capabilityMask') END,
			       COALESCE(allow_secret_injection, 0)
			  FROM runners WHERE id = ?`, id).Scan(&name, &capsJSON, &mask, &injection)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Not registered. A binding already on the scope keeps its recorded
			// name; anything else is just an id the save would refuse.
			if b, ok := already[id]; ok {
				pr.Name = b.Name
			}
			out.Runners = append(out.Runners, pr)
			continue
		case err != nil:
			return nil, fmt.Errorf("preview scope runners: %w", err)
		}
		pr.Name, pr.Registered, pr.AllowsSecretInjection = name, true, injection == 1
		if pr.Eligible, err = execspec.RunnerEligibleForScope(ctx, database, scopeID, id); err != nil {
			return nil, fmt.Errorf("preview scope runners: %w", err)
		}
		var caps, masked []string
		_ = json.Unmarshal([]byte(capsJSON), &caps)
		if mask.Valid {
			_ = json.Unmarshal([]byte(mask.String), &masked)
		}
		drop := make(map[string]bool, len(masked))
		for _, m := range masked {
			drop[m] = true
		}
		have := map[string]bool{}
		for _, c := range caps {
			if !drop[c] {
				pr.Capabilities = append(pr.Capabilities, c)
				have[c] = true
			}
		}
		for _, rt := range out.RunTypes {
			if !have[rt] {
				pr.MissingRunTypes = append(pr.MissingRunTypes, rt)
			}
		}
		out.Runners = append(out.Runners, pr)
	}
	return out, nil
}
