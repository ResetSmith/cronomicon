package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// v0.56.4 — the release where an unscoped job stops carrying authority of its own
// (RB-26), its declared target_host stops being unvalidated (RB-27), and the
// scheduled-run bypass that RB-Q11(c) deliberately leaves open gets the guard that
// keeps it from becoming an escalation (RB-30).

// TestUnscopedJobRequiresBoundScope — RB-26. The rule: a restricted actor may
// trigger an unscoped job, but must name an effective scope they hold; only an
// unrestricted actor runs it unbound.
//
// This is what turns an unscoped job into a TEMPLATE — one restart-service, run by
// Tax against Tax and Finance against Finance — rather than a hole through which
// "scoped to Tax" stops being true.
func TestUnscopedJobRequiresBoundScope(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// `tax` and its agency come from the fixture (restrict{"operator":"tax"}); only
	// the scope the operator does NOT hold needs adding.
	seed(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('s-fin','finance','amadeus','2026-01-01T00:00:00Z')`)
	// The template: no scope of its own.
	seed(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('restart-service','git','bash',NULL,1)`)

	jobID := runPath(t, pool, "restart-service")
	run := func(group, body string) int {
		t.Helper()
		return reqAs(t, h, http.MethodPost, jobID, group, body).Code
	}

	// A restricted operator with NO bound scope: denied, and told what to do.
	rec := reqAs(t, h, http.MethodPost, jobID, "sec-operators", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted actor, unbound = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != "scope_required" {
		t.Errorf("error code = %q, want scope_required", errBody.Code)
	}
	// A 403 on a job that worked yesterday must say what to DO, not just deny.
	if errBody.Message == "" || !containsAll(errBody.Message, "scope") {
		t.Errorf("denial message %q does not tell the operator to choose a scope", errBody.Message)
	}

	// Binding a scope they HOLD: allowed.
	if code := run("sec-operators", `{"scope":"tax"}`); code != http.StatusAccepted {
		t.Errorf("restricted actor binding a held scope = %d, want 202", code)
	}
	// Binding a scope they do NOT hold: denied. Otherwise the bind would be a
	// formality anyone could satisfy with any string.
	if code := run("sec-operators", `{"scope":"finance"}`); code != http.StatusForbidden {
		t.Errorf("restricted actor binding an UNHELD scope = %d, want 403", code)
	}
	// An unrestricted actor still runs it unbound — the genuinely global case
	// ("back up the Cronomicon DB") that §2.5 preserves.
	if code := run("sec-admins", ""); code != http.StatusAccepted {
		t.Errorf("unrestricted actor, unbound = %d, want 202", code)
	}
}

// TestUnscopedJobBindRecordsScopeAndAgencies — RB-26's side effects, which are a
// correctness win independent of authorization: a bound run leaves the general pool
// and is claimable by that scope's agency runners.
func TestUnscopedJobBindRecordsScopeAndAgencies(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('restart-service','git','bash',NULL,1)`)

	if rec := reqAs(t, h, http.MethodPost, runPath(t, pool, "restart-service"), "sec-operators",
		`{"scope":"tax"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("bound run = %d (%s)", rec.Code, rec.Body.String())
	}

	var scope, agenciesJSON string
	if err := pool.QueryRow(
		`SELECT COALESCE(scope,''), COALESCE(agencies_json,'') FROM runs WHERE job_name='restart-service'`).
		Scan(&scope, &agenciesJSON); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if scope != "tax" {
		t.Errorf("run.scope = %q, want the BOUND scope tax — the run must record what it was bound to", scope)
	}
	// The dispatch win: an unbound run reaches only runners in no agency (AG-Q3a).
	// A bound one derives the agency set and reaches that department's fleet.
	if !containsAll(agenciesJSON, "agency-tax") {
		t.Errorf("run.agencies_json = %q, want the bound scope's agency set — a bound run "+
			"must leave the general pool", agenciesJSON)
	}
}

// TestDeclaredTargetHostValidatedAgainstBoundScope — RB-27, the prerequisite
// without which RB-26 is enforcement theatre.
//
// Per-run host SUBSETS were always validated against the scope's inventory; a job's
// DECLARED target_host never was. So an unscoped job pinned to a Finance host could
// be bound to Tax and still reach Finance — the scope the actor was forced to name
// would mean precisely nothing.
func TestDeclaredTargetHostValidatedAgainstBoundScope(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:tax','tax-web-01')`)
	// Unscoped, but pinned to a host that belongs to another department.
	seed(`INSERT INTO jobs (name, source, run_type, scope, target_host, enabled)
	      VALUES ('restart-service','git','bash',NULL,'finance-db-01',1)`)

	rec := reqAs(t, h, http.MethodPost, runPath(t, pool, "restart-service"), "sec-operators", `{"scope":"tax"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("binding tax to a job pinned to finance-db-01 = %d, want 422 — otherwise the "+
			"bind is decorative and the run still reaches Finance (%s)", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	// Same code as the subset path, so the error vocabulary stays uniform.
	if errBody.Code != "scope_membership" {
		t.Errorf("error code = %q, want scope_membership (matching the host-subset path)", errBody.Code)
	}

	// An unrestricted actor is unaffected — they already reach every host.
	if rec := reqAs(t, h, http.MethodPost, runPath(t, pool, "restart-service"), "sec-admins", ""); rec.Code != http.StatusAccepted {
		t.Errorf("unrestricted actor = %d, want 202 (RB-27 must not touch them)", rec.Code)
	}
}

// TestScheduleDefsAdminOnly_GuardsUnscopedSchedulingBypass — RB-30.
//
// THIS TEST IS THE ENFORCEMENT OF A DECISION, not a route check. RB-Q11(c) leaves a
// scheduled fire of an unscoped job unbound and unchecked, because a cron fire has
// no actor and inventing one would be fiction. That is safe ONLY because authoring a
// schedule requires admin. Nothing in the code said so — so the day compose widens
// beyond admin (which requireCompose's own note already contemplates), the bypass
// becomes a real escalation silently, in a change whose author has no reason to be
// thinking about the scheduler.
//
// Named for the REASON so that widening compose fails a test which explains why it
// matters. If you are here because this failed: you did not break scheduling, you
// removed the assumption that made RB-Q11(c) acceptable. Either keep the gate, or
// implement the deferred per-entry schedule scope (§10) first.
func TestScheduleDefsAdminOnly_GuardsUnscopedSchedulingBypass(t *testing.T) {
	h, _ := secretRBACServer(t, nil)

	body := `{"name":"nightly","cron":"0 2 * * *"}`
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/schedule-defs", body},
		{http.MethodPut, "/api/v1/schedule-defs/nightly", body},
		{http.MethodDelete, "/api/v1/schedule-defs/nightly", ""},
	} {
		if rec := reqAs(t, h, tc.method, tc.path, "sec-viewers", tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin %s %s = %d, want 403.\n"+
				"Schedule authoring MUST stay admin-only: a scheduled fire of an unscoped job "+
				"runs unbound with no scope check (RB-Q11(c)), so anyone who can author a "+
				"schedule could walk around RB-26's binding rule.",
				tc.method, tc.path, rec.Code)
		}
	}
}

// runPath is the run URL for a job, addressed by rowid as the route expects.
func runPath(t *testing.T, pool *sql.DB, name string) string {
	t.Helper()
	return "/api/v1/jobs/" + strconv.FormatInt(jobRowID(t, pool, name), 10) + "/run"
}

func containsAll(hay string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(hay); i++ {
			if hay[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestDeferredRunsAreGatedAndFreezeTheirScope — RB-31, the CREATION half.
//
// A deferred trigger is validated at creation and replayed at promotion with no
// identity attached and no re-check, so creation is the only place authorization
// happens: the deferral branch sits after the run-path guards, which is what
// makes RB-2 and RB-26 apply to it, and the bound scope is frozen onto the row
// because that is what promotion replays.
//
// ⚠️ Renamed in RF-11 (the RBAC-fixes plan). It used to be called
// …CarryFrozenAuthorization, which over-promised: it never revoked anything and
// never promoted, so RB-Q12's actual decision — a parked run fires even after its
// creator's grants are gone — was asserted nowhere. That property lives where it
// happens, in scheduler.PromotePending; see
// TestRevokedCreatorsPendingRunStillFires in internal/scheduler.
func TestDeferredRunsAreGatedAndFreezeTheirScope(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// `tax` and its agency come from the fixture (restrict{"operator":"tax"}).
	seed(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('restart-service','git','bash',NULL,1)`)
	path := runPath(t, pool, "restart-service")

	// Deferral does not bypass RB-26: a viewer cannot park what they cannot run.
	if rec := reqAs(t, h, http.MethodPost, path, "sec-viewers",
		`{"runAt":"`+futureRunAt()+`","scope":"tax"}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer deferring a run = %d, want 403 — scheduling for later must "+
			"require exactly the power of running now, time-shifted", rec.Code)
	}
	// …and an unbound deferral of an unscoped job is refused for the same reason
	// an immediate one is.
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators",
		`{"runAt":"`+futureRunAt()+`"}`); rec.Code != http.StatusForbidden {
		t.Errorf("restricted actor deferring an UNBOUND run = %d, want 403 (RB-26)", rec.Code)
	}

	// A properly bound deferral is accepted and FREEZES the bound scope, which is
	// what the promotion loop replays — the run fires into tax, not into nothing.
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators",
		`{"runAt":"`+futureRunAt()+`","scope":"tax"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("bound deferral = %d (%s)", rec.Code, rec.Body.String())
	}
	var frozenScope, by string
	if err := pool.QueryRow(
		`SELECT COALESCE(scope,''), scheduled_by FROM pending_runs WHERE name='restart-service'`).
		Scan(&frozenScope, &by); err != nil {
		t.Fatalf("read pending row: %v", err)
	}
	if frozenScope != "tax" {
		t.Errorf("pending_runs.scope = %q, want the frozen bound scope tax — promotion "+
			"replays this, so an empty value would fire unbound", frozenScope)
	}
	// The creator is on the row, which is what makes the RB-Q12 manual sweep
	// possible at all: without it an operator could not tell whose parked runs to
	// cancel after a revocation.
	if by == "" {
		t.Error("pending_runs.scheduled_by is empty — the RB-Q12 recourse is a by-creator sweep")
	}
}

// TestDeferredWorkflowTriggerIsAuthorizedAtCreation — RF-11, the deferred
// WORKFLOW path (the RBAC-fixes plan).
//
// It inserts with scope "" and nil params, which reads like a hole and is not:
// a workflow has no scope of its own, the engine re-resolves each step at fire
// time, and authorization ran over every constituent scope BEFORE the deferral
// branch. That ordering is the whole safety argument, so it needs a test — the
// existing coverage of this path (TestDeferredWorkflowTrigger) exercises the
// mechanics with an admin and would not notice if the guard moved below the
// branch.
func TestDeferredWorkflowTriggerIsAuthorizedAtCreation(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT OR IGNORE INTO scopes (id,name,source,created_at)
	      VALUES ('s-fin','finance','amadeus','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('step-tax','amadeus','bash','tax',1)`)
	seed(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('step-fin','amadeus','bash','finance',1)`)
	seed(`INSERT INTO workflows (name, source, steps, enabled, synced_at)
	      VALUES ('wf-tax','amadeus','[{"type":"job","name":"step-tax"}]',1,'t')`)
	// A workflow reaching into a department the operator does NOT hold.
	seed(`INSERT INTO workflows (name, source, steps, enabled, synced_at)
	      VALUES ('wf-mixed','amadeus','[{"type":"job","name":"step-tax"},{"type":"job","name":"step-fin"}]',1,'t')`)

	trigger := func(name string) string {
		var rowid int64
		if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE name = ?`, name).Scan(&rowid); err != nil {
			t.Fatalf("rowid %s: %v", name, err)
		}
		return "/api/v1/workflows/" + strconv.FormatInt(rowid, 10) + "/trigger"
	}
	later := `{"runAt":"` + futureRunAt() + `"}`

	// A viewer holds no verb: deferring is refused exactly as running now is.
	if rec := reqAs(t, h, http.MethodPost, trigger("wf-tax"), "sec-viewers", later); rec.Code != http.StatusForbidden {
		t.Errorf("viewer deferring a workflow = %d, want 403 — parking a trigger must "+
			"require the power to fire it", rec.Code)
	}
	// One constituent scope out of reach is enough to refuse the whole workflow,
	// and deferral does not launder that.
	if rec := reqAs(t, h, http.MethodPost, trigger("wf-mixed"), "sec-operators", later); rec.Code != http.StatusForbidden {
		t.Errorf("tax operator deferring a workflow with a FINANCE step = %d, want 403", rec.Code)
	}
	// Their own department's workflow parks, with the creator recorded for the
	// RB-Q12 sweep.
	if rec := reqAs(t, h, http.MethodPost, trigger("wf-tax"), "sec-operators", later); rec.Code != http.StatusAccepted {
		t.Fatalf("tax operator deferring their own workflow = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	var by string
	if err := pool.QueryRow(
		`SELECT scheduled_by FROM pending_runs WHERE kind='workflow' AND name='wf-tax'`).Scan(&by); err != nil {
		t.Fatalf("read parked workflow row: %v", err)
	}
	if by == "" {
		t.Error("parked workflow row has no scheduled_by — the RB-Q12 sweep needs it")
	}
}

// futureRunAt is a deferral instant that stays in the future — a literal date
// rotted into the past once and turned two authz tests into 422s.
func futureRunAt() string { return time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339) }
