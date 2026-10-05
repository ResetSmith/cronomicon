package gitlab

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

// TestSyncWarnsWhenAJobAsksForSSHOnABoundScope: a git job with `executor: ssh`
// on a scope bound to runners is refused at every fire, and nobody watches a
// cron fire — so sync says it, by job and scope, as soon as it can see both
// facts. The same job on a scope nobody bound draws no warning, and neither
// does a job that sets no executor (it simply runs on the bound runners).
func TestSyncWarnsWhenAJobAsksForSSHOnABoundScope(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	var logs bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))

	job := func(name, scope, executor string) string {
		y := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  run_type: bash\n  command: echo hi\n  scope: " + scope + "\n"
		if executor != "" {
			y += "  executor: " + executor + "\n"
		}
		return y
	}
	gitCommitFile(t, repo, remote, "inventory/dmz.ini", "[web]\nweb1\n", "add dmz")
	gitCommitFile(t, repo, remote, "inventory/open.ini", "[app]\napp1\n", "add open")
	gitCommitFile(t, repo, remote, "jobs/legacy.yaml", job("legacy", "dmz", "ssh"), "add legacy")
	gitCommitFile(t, repo, remote, "jobs/plain.yaml", job("plain", "dmz", ""), "add plain")
	gitCommitFile(t, repo, remote, "jobs/elsewhere.yaml", job("elsewhere", "open", "ssh"), "add elsewhere")
	// A job that takes its body from a script takes its EXECUTOR from it too, so
	// the stored executor — the one a fire reads — is the script's. `inherits`
	// says nothing itself and is refused through its script; `overridden` says
	// ssh itself, which is discarded in favour of a script that says nothing, so
	// it simply runs on the bound runners and must not be warned about.
	script := func(name, executor string) string {
		y := "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: " + name +
			"\nspec:\n  run_type: bash\n  command: echo hi\n"
		if executor != "" {
			y += "  executor: " + executor + "\n"
		}
		return y
	}
	refJob := func(name, ref, executor string) string {
		y := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  script_ref: " + ref + "\n  scope: dmz\n"
		if executor != "" {
			y += "  executor: " + executor + "\n"
		}
		return y
	}
	gitCommitFile(t, repo, remote, "scripts/over-ssh.yaml", script("over-ssh", "ssh"), "add over-ssh")
	gitCommitFile(t, repo, remote, "scripts/no-opinion.yaml", script("no-opinion", ""), "add no-opinion")
	gitCommitFile(t, repo, remote, "jobs/inherits.yaml", refJob("inherits", "over-ssh", ""), "add inherits")
	gitCommitFile(t, repo, remote, "jobs/overridden.yaml", refJob("overridden", "no-opinion", "ssh"), "add overridden")

	const warning = "asks for the ssh executor on a scope bound to runners"
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("initial sync failed: %s", r.ErrorMessage)
	}
	if strings.Contains(logs.String(), warning) {
		t.Fatalf("warned before any scope was bound:\n%s", logs.String())
	}

	if _, err := svc.db.Exec(`
		INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
		SELECT id, 'r-dmz', 'runner-dmz-01', 'ops@example', 'now' FROM scopes WHERE name='dmz'`); err != nil {
		t.Fatalf("bind: %v", err)
	}
	logs.Reset()
	// A warning is advice: the sync must still be a clean success, not "partial"
	// (which would also suppress pruning for the whole subsystem).
	if r := svc.SyncBlocking(ctx, "t"); r.Status != "success" {
		t.Fatalf("sync after binding = %q, want success (%s)", r.Status, r.ErrorMessage)
	}
	warned := map[string]bool{}
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, warning) {
			continue
		}
		if !strings.Contains(line, "scope=dmz") {
			t.Errorf("warning does not name the scope: %s", line)
		}
		for _, name := range []string{"legacy", "plain", "elsewhere", "inherits", "overridden"} {
			if strings.Contains(line, "job="+name+" ") {
				warned[name] = true
			}
		}
	}
	if len(warned) != 2 || !warned["legacy"] || !warned["inherits"] {
		t.Errorf("warned jobs = %v, want exactly legacy (its own executor) and inherits (its script's)", warned)
	}
}
