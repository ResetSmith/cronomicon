package gitlab

import (
	"context"
	"testing"
)

// TestSyncHoldsABoundScopeWithWaitingRuns drives the REAL sync (clone, parse,
// upsert, prune) to pin two things about scope↔runner bindings (SB-1):
//
//   - an ordinary re-sync leaves a binding alone, through the whole pipeline and
//     not just the upsert statement (TestSyncPreservesScopeRunners covers that);
//   - a scope removed from Git is NOT pruned while it is bound and has runs
//     waiting under its name. A run carries its scope by name: with the row
//     gone it reads as unrestricted and any runner in its agency may claim it.
//     The prune is deferred, not refused — the scope goes on the first sync
//     after the queue drains, and its binding goes with it.
//
// An unbound scope removed in the same commit is pruned at once, so the hold is
// shown to be about the binding and not about scopes in general.
func TestSyncHoldsABoundScopeWithWaitingRuns(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	pool := svc.db

	gitCommitFile(t, repo, remote, "inventory/dmz.ini", "[web]\nweb1\n", "add dmz")
	gitCommitFile(t, repo, remote, "inventory/plain.ini", "[app]\napp1\n", "add plain")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("initial sync failed: %s", r.ErrorMessage)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v\n%s", err, q)
		}
		return n
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	backdateScopes := func() {
		exec(`UPDATE scopes SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`)
	}
	if count(`SELECT COUNT(*) FROM scopes WHERE name IN ('dmz','plain') AND source='git'`) != 2 {
		t.Fatal("dmz and plain were not imported by the initial sync")
	}

	// The operator binds dmz; a run is waiting on it.
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
	      SELECT id, 'r-dmz', 'runner-dmz-01', 'ops@example', 'now' FROM scopes WHERE name='dmz'`)
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES ('run-waiting', 'deploy', 'bash', 'dmz', 'queued', 'seed', 'manual', 'runner', '2026-10-05T00:00:00Z')`)

	// An ordinary re-sync: nothing removed, the binding is untouched.
	backdateScopes()
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("re-sync failed: %s", r.ErrorMessage)
	}
	if count(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-dmz'`) != 1 {
		t.Fatal("a re-sync that removed nothing dropped the scope's binding")
	}

	// Both inventories leave Git.
	gitRemoveFile(t, repo, "inventory/dmz.ini", "remove dmz")
	gitRemoveFile(t, repo, "inventory/plain.ini", "remove plain")
	backdateScopes()
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync after removal failed: %s", r.ErrorMessage)
	}
	if count(`SELECT COUNT(*) FROM scopes WHERE name='plain'`) != 0 {
		t.Error("the unbound scope was not pruned on a clean sync")
	}
	if count(`SELECT COUNT(*) FROM scopes WHERE name='dmz'`) != 1 {
		t.Fatal("the bound scope was pruned while a run was waiting under its name")
	}
	if count(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-dmz'`) != 1 {
		t.Error("the held scope lost its binding")
	}
	if count(`SELECT COUNT(*) FROM scope_hosts sh JOIN scopes sc ON sc.id = sh.scope_id WHERE sc.name='dmz'`) == 0 {
		t.Error("the held scope lost its hosts — it must be kept whole, not as an empty shell")
	}

	// The queue drains; the next sync prunes the scope and the binding with it.
	exec(`UPDATE runs SET status='success' WHERE id='run-waiting'`)
	backdateScopes()
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync after the queue drained failed: %s", r.ErrorMessage)
	}
	if count(`SELECT COUNT(*) FROM scopes WHERE name='dmz'`) != 0 {
		t.Error("the scope was not pruned once nothing was waiting on it")
	}
	if count(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-dmz'`) != 0 {
		t.Error("the pruned scope's binding was left behind")
	}
}
