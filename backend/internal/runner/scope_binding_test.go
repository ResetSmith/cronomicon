package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// SB-1 — scope↔runner bindings (the scope-bound-runners plan, mig. 1180).
//
// These tests hold the lines the binding is easiest to get wrong on:
//   - a scope with NO binding must dispatch exactly as it did before (the
//     feature is opt-in per scope, and a regression here breaks every job);
//   - a binding NARROWS and never widens — it is ANDed with agency eligibility,
//     so it can never reach a runner agency isolation denies;
//   - a binding OUTLIVES its runner — deleting the runner row must leave the
//     scope closed, not open it to the rest of the agency;
//   - the rule is read at claim time, so a queued run follows a runner swap.

func insertScopeRow(t *testing.T, svc *Service, id, name string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO scopes(id, name, source, created_at) VALUES(?, ?, 'cronomicon', ?)`, id, name, now()); err != nil {
		t.Fatalf("insertScopeRow: %v", err)
	}
}

// bindScope writes a binding the way the API does: the runner's id plus the name
// snapshotted beside it. runnerName is passed rather than looked up so a test
// can bind a runner that has no row at all.
func bindScope(t *testing.T, svc *Service, scopeID, runnerID, runnerName string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at) VALUES(?, ?, ?, 'test', ?)`,
		scopeID, runnerID, runnerName, now()); err != nil {
		t.Fatalf("bindScope: %v", err)
	}
}

// insertQueuedRunScoped inserts a queued runner run on a scope (by name; "" is
// a run with no scope) with an optional agency. agency=="" is the general pool.
func insertQueuedRunScoped(t *testing.T, svc *Service, traceID, scope, agency string) {
	t.Helper()
	aj := "[]"
	if agency != "" {
		b, _ := json.Marshal([]string{agency})
		aj = string(b)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		VALUES (?, 'j', 'bash', ?, 'queued', 'test', 'manual', 'runner', ?, ?)`,
		traceID, nullIfBlank(scope), aj, now()); err != nil {
		t.Fatalf("insertQueuedRunScoped: %v", err)
	}
	if agency != "" {
		if _, err := svc.db.Exec(`INSERT INTO run_agencies(run_id, agency) VALUES(?,?)`, traceID, agency); err != nil {
			t.Fatalf("insertQueuedRunScoped: materialize: %v", err)
		}
	}
}

// claims reports whether runnerID claims a fresh run on scope/agency. Each call
// uses its own run and removes it, so no case can bleed into the next.
func claims(t *testing.T, svc *Service, scope, agency, runnerID string) bool {
	t.Helper()
	trace := db.NewTraceID()
	insertQueuedRunScoped(t, svc, trace, scope, agency)
	got, err := svc.claimRun(context.Background(), runnerID, []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	claimed := got != nil && got.TraceID == trace
	_, _ = svc.db.Exec(`DELETE FROM runs WHERE id=?`, trace)
	return claimed
}

// TestClaimRunScopeBindingMatrix is SB-1's keystone.
func TestClaimRunScopeBindingMatrix(t *testing.T) {
	svc := newTestService(t)

	insertRunner(t, svc, "r-dmz-1", "dmz-01", "online", []string{"bash"})
	insertRunner(t, svc, "r-dmz-2", "dmz-02", "online", []string{"bash"})
	insertRunner(t, svc, "r-plain", "plain", "online", []string{"bash"})
	insertScopeRow(t, svc, "s-dmz", "dmz-web")
	insertScopeRow(t, svc, "s-open", "open")
	bindScope(t, svc, "s-dmz", "r-dmz-1", "dmz-01")
	bindScope(t, svc, "s-dmz", "r-dmz-2", "dmz-02")

	cases := []struct {
		name, scope, runner string
		want                bool
	}{
		// The no-regression cases. If any of these fails, the feature is not
		// opt-in and every scope in the fleet is affected.
		{"unbound scope → any runner", "open", "r-plain", true},
		{"unbound scope → a runner bound elsewhere", "open", "r-dmz-1", true},
		{"no scope → any runner", "", "r-plain", true},
		{"a scope name with no scopes row → any runner", "never-created", "r-plain", true},

		{"bound scope → a named runner", "dmz-web", "r-dmz-1", true},
		{"bound scope → the other named runner", "dmz-web", "r-dmz-2", true},
		{"bound scope → a runner not named", "dmz-web", "r-plain", false},
	}
	for _, c := range cases {
		if got := claims(t, svc, c.scope, "", c.runner); got != c.want {
			t.Errorf("%s: claimed=%v want=%v", c.name, got, c.want)
		}
	}
}

// TestClaimRunScopeBindingNarrowsNeverWidens is the RT-Q2 invariant carried over
// to bindings. A run in agency alpha on a bound scope must be claimable ONLY by
// a runner that is both a member of alpha and named — and in particular a named
// runner in the WRONG agency must still be refused. Writing the binding as an
// OR, or folding it inside the agency parenthesis, passes the matrix above and
// fails precisely here.
func TestClaimRunScopeBindingNarrowsNeverWidens(t *testing.T) {
	svc := newTestService(t)

	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")
	insertRunner(t, svc, "r-alpha-bound", "alpha-bound", "online", []string{"bash"})
	insertRunner(t, svc, "r-beta-bound", "beta-bound", "online", []string{"bash"})
	insertRunner(t, svc, "r-alpha-plain", "alpha-plain", "online", []string{"bash"})
	addRunnerAgency(t, svc, "r-alpha-bound", "a1")
	addRunnerAgency(t, svc, "r-beta-bound", "a2")
	addRunnerAgency(t, svc, "r-alpha-plain", "a1")
	insertScopeRow(t, svc, "s-dmz", "dmz-web")
	bindScope(t, svc, "s-dmz", "r-alpha-bound", "alpha-bound")
	bindScope(t, svc, "s-dmz", "r-beta-bound", "beta-bound")

	for runner, want := range map[string]bool{
		"r-alpha-bound": true,
		"r-beta-bound":  false, // named, but agency isolation denies it
		"r-alpha-plain": false, // in the agency, but not named
	} {
		if got := claims(t, svc, "dmz-web", "alpha", runner); got != want {
			t.Errorf("%s: claimed=%v want=%v", runner, got, want)
		}
	}
}

// TestClaimRunBindingOutlivesItsRunner pins the fail-closed rule. A binding
// whose runner row is gone — deregistered by an operator, or reaped after an
// outage — must keep the scope restricted. If it did not, losing the one runner
// that reaches a network segment would hand that segment's jobs to every other
// runner in the agency.
func TestClaimRunBindingOutlivesItsRunner(t *testing.T) {
	svc := newTestService(t)

	insertRunner(t, svc, "r-dmz", "dmz-01", "online", []string{"bash"})
	insertRunner(t, svc, "r-plain", "plain", "online", []string{"bash"})
	insertScopeRow(t, svc, "s-dmz", "dmz-web")
	bindScope(t, svc, "s-dmz", "r-dmz", "dmz-01")

	if _, err := svc.db.Exec(`DELETE FROM runners WHERE id = 'r-dmz'`); err != nil {
		t.Fatalf("delete runner: %v", err)
	}
	var rows int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id = 'r-dmz'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("binding rows after deleting the runner = %d (err %v), want 1", rows, err)
	}
	if claims(t, svc, "dmz-web", "", "r-plain") {
		t.Error("a runner that was never named claimed a run on a scope whose only bound runner is gone")
	}
}

// TestClaimRunFollowsARunnerSwap pins that the binding is read at claim time. A
// run queued while the scope was bound to one runner must be claimable by its
// replacement the moment the binding changes — the binding is not snapshotted
// onto the run the way agencies are, precisely so a swap strands nothing.
func TestClaimRunFollowsARunnerSwap(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertRunner(t, svc, "r-old", "dmz-01", "online", []string{"bash"})
	insertRunner(t, svc, "r-new", "dmz-02", "online", []string{"bash"})
	insertScopeRow(t, svc, "s-dmz", "dmz-web")
	bindScope(t, svc, "s-dmz", "r-old", "dmz-01")

	trace := db.NewTraceID()
	insertQueuedRunScoped(t, svc, trace, "dmz-web", "")
	if got, err := svc.claimRun(ctx, "r-new", []string{"bash"}, true); err != nil || got != nil {
		t.Fatalf("before the swap r-new claimed %v (err %v), want nothing", got, err)
	}

	if _, err := svc.db.Exec(`UPDATE scope_runners SET runner_id = 'r-new', runner_name = 'dmz-02' WHERE runner_id = 'r-old'`); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got, err := svc.claimRun(ctx, "r-new", []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun after the swap: %v", err)
	}
	if got == nil || got.TraceID != trace {
		t.Fatalf("after the swap r-new claimed %v, want the already-queued run", got)
	}
}

// TestWatchesFollowScopeBinding: a bound scope's file watches are distributed
// only to the runners it names. The drop directory is on a host those runners
// were placed to reach; the same path polled from another segment is at best
// wasted scans and at worst a different file.
func TestWatchesFollowScopeBinding(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertRunner(t, svc, "r-dmz", "dmz-01", "online", []string{"bash", "watch"})
	insertRunner(t, svc, "r-plain", "plain", "online", []string{"bash", "watch"})
	insertScopeRow(t, svc, "s-dmz", "dmz-web")
	insertScopeRow(t, svc, "s-open", "open")
	bindScope(t, svc, "s-dmz", "r-dmz", "dmz-01")
	seedWatchJob(t, svc, "cronomicon", "bound-ingest", "dmz-web", `[{"path":"/drop/dmz/*.csv"}]`)
	seedWatchJob(t, svc, "cronomicon", "open-ingest", "open", `[{"path":"/drop/open/*.csv"}]`)

	names := func(runnerID string) map[string]bool {
		out := map[string]bool{}
		for _, w := range watchesForRunner(ctx, svc.db, runnerID, []string{"bash", "watch"}) {
			out[w.JobName] = true
		}
		return out
	}
	if got := names("r-dmz"); !got["bound-ingest"] || !got["open-ingest"] {
		t.Errorf("the named runner watches %v, want both jobs", got)
	}
	if got := names("r-plain"); got["bound-ingest"] || !got["open-ingest"] {
		t.Errorf("a runner not named watches %v, want only the unbound scope's job", got)
	}
}

// TestDeregisterAndReaperLeaveBindingsInPlace runs the two REAL deletion paths
// (operator deregister and the reaper's deregisterRunner) rather than a bare
// DELETE, so a cascade or a tidy-up added to either one is caught here. The
// membership rows go; the binding stays, and with it the scope's restriction.
func TestDeregisterAndReaperLeaveBindingsInPlace(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "r-op", "runner-op")
	seedPlacedRunner(t, svc, "r-reaped", "runner-reaped")
	insertScopeRow(t, svc, "s-op", "by-operator")
	insertScopeRow(t, svc, "s-reaped", "by-reaper")
	bindScope(t, svc, "s-op", "r-op", "runner-op")
	bindScope(t, svc, "s-reaped", "r-reaped", "runner-reaped")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/r-op", nil)
	req.SetPathValue("id", "r-op")
	rec := httptest.NewRecorder()
	svc.HandleDeregisterRunner(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deregister = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	svc.deregisterRunner(context.Background(), "r-reaped", "runner-reaped")

	for _, id := range []string{"r-op", "r-reaped"} {
		var runners, bindings int
		if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runners WHERE id = ?`, id).Scan(&runners); err != nil {
			t.Fatal(err)
		}
		if err := svc.db.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id = ?`, id).Scan(&bindings); err != nil {
			t.Fatal(err)
		}
		if runners != 0 {
			t.Errorf("%s: runner row still present", id)
		}
		if bindings != 1 {
			t.Errorf("%s: %d binding rows after deletion, want 1 (the scope must stay closed)", id, bindings)
		}
	}
}

// TestApplyPlacementRepointsScopeBindings: a re-enrolled runner has a new id, so
// the bindings its predecessor left behind name nobody. The offer says which
// scopes are waiting, and accepting it re-points them in the same transaction
// that restores the agencies making the runner eligible for them.
func TestApplyPlacementRepointsScopeBindings(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	insertScopeRow(t, svc, "s-a", "alpha-hosts")
	insertScopeRow(t, svc, "s-b", "beta-hosts")
	bindScope(t, svc, "s-a", "old", "ansible-rh8")
	bindScope(t, svc, "s-b", "old", "ansible-rh8")
	svc.deregisterRunner(ctx, "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")
	// An operator already bound one of the scopes to the new runner by hand; the
	// re-point must absorb that rather than fail on the primary key.
	bindScope(t, svc, "s-b", "new", "ansible-rh8")

	got, err := suggestionsFor(ctx, svc.db, map[string]string{"new": "ansible-rh8"})
	if err != nil {
		t.Fatal(err)
	}
	if s := got["new"]; s == nil || len(s.Scopes) != 2 || s.Scopes[0] != "alpha-hosts" || s.Scopes[1] != "beta-hosts" {
		t.Fatalf("suggestion scopes = %+v, want both scopes the old id still holds", got["new"])
	}

	var histID int64
	if err := svc.db.QueryRow(`SELECT id FROM runner_placement_history WHERE runner_id='old'`).Scan(&histID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyPlacement(ctx, "new", histID, "ops@example", func(string) bool { return true }); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var onOld, onNew int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='old'`).Scan(&onOld); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='new'`).Scan(&onNew); err != nil {
		t.Fatal(err)
	}
	if onOld != 0 || onNew != 2 {
		t.Errorf("bindings after restore: old=%d new=%d, want 0 and 2", onOld, onNew)
	}
	found := false
	for _, row := range activityRows(t, svc, "ansible-rh8") {
		if strings.Contains(row, "alpha-hosts") && strings.Contains(row, "beta-hosts") {
			found = true
		}
	}
	if !found {
		t.Error("the restore's activity row does not name the scopes it re-pointed")
	}
}

// TestApplyPlacementRestoresAGeneralPoolRunnersBindings: a general-pool runner
// has no agencies, so its placement snapshot is empty and — before SB-1 — earned
// no offer at all. Its scope bindings ARE its placement: without an offer the
// scopes it served stay closed with no one-click way back.
func TestApplyPlacementRestoresAGeneralPoolRunnersBindings(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "old", "pool-runner", "online", []string{"bash"})
	insertScopeRow(t, svc, "s-shared", "shared-hosts")
	bindScope(t, svc, "s-shared", "old", "pool-runner")
	svc.deregisterRunner(ctx, "old", "pool-runner")
	reRegister(t, svc, "new", "pool-runner", "10.142.11.9")

	got, err := suggestionsFor(ctx, svc.db, map[string]string{"new": "pool-runner"})
	if err != nil {
		t.Fatal(err)
	}
	s := got["new"]
	if s == nil {
		t.Fatal("no offer for a re-enrolled general-pool runner whose old id still holds a scope binding")
	}
	if len(s.Agencies) != 0 || len(s.Scopes) != 1 || s.Scopes[0] != "shared-hosts" {
		t.Errorf("offer = agencies %v scopes %v, want no agencies and [shared-hosts]", s.Agencies, s.Scopes)
	}

	if err := svc.ApplyPlacement(ctx, "new", s.HistoryID, "ops@example", func(string) bool { return true }); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var onNew int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE scope_id='s-shared' AND runner_id='new'`).Scan(&onNew); err != nil || onNew != 1 {
		t.Errorf("bindings on the re-enrolled runner = %d (err %v), want 1", onNew, err)
	}
	if !claims(t, svc, "shared-hosts", "", "new") {
		t.Error("the restored general-pool runner cannot claim a run on the scope it was re-pointed to")
	}

	// With no agencies AND no bindings left, there is nothing to restore: no
	// offer, and the same snapshot can no longer be applied.
	svc.deregisterRunner(ctx, "new", "pool-runner")
	if _, err := svc.db.Exec(`DELETE FROM scope_runners`); err != nil {
		t.Fatal(err)
	}
	reRegister(t, svc, "newer", "pool-runner", "10.142.11.9")
	got, err = suggestionsFor(ctx, svc.db, map[string]string{"newer": "pool-runner"})
	if err != nil {
		t.Fatal(err)
	}
	if got["newer"] != nil {
		t.Errorf("offer = %+v for a general-pool runner with nothing to restore, want none", got["newer"])
	}
	if err := svc.ApplyPlacement(ctx, "newer", s.HistoryID, "ops@example", func(string) bool { return true }); err != ErrPlacementGone {
		t.Errorf("apply with nothing to restore = %v, want ErrPlacementGone", err)
	}
}
