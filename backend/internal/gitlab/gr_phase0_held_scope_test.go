package gitlab

import (
	"fmt"
	"testing"
)

// Present defect 8, read by Phase R0 and reproduced here before Phase R3
// rewrites the prunes. It passes on the code as it is.
//
// A scope that leaves Git while it is bound to runners and has runs waiting is
// held back from the prune "hosts and all" (the comment over the prune says
// so): the runs waiting under its name must still find it whole. Its host
// MEMBERSHIP is kept. Its imported host RECORDS (the address, port, user and
// key a host is reached by, in ssh_hosts) are not: their prune asks neither
// which scope a record belongs to nor whether that scope is being held, so
// they go at the sync that held the scope. The run that was waiting is then
// dispatched to hosts the server no longer knows how to reach.
func TestGR0_AHeldScopeLosesItsImportedHostRecords(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	gitCommitFile(t, repo, remote, "inventory/dmz.ini", fmt.Sprintf(grScope, "web1", "10.0.0.1"), "add dmz")
	grSync(t, svc, "first sync")
	const records = `SELECT COUNT(*) FROM ssh_hosts h JOIN scopes sc ON sc.id = h.scope_id WHERE sc.name = 'dmz' AND h.source = 'git'`
	const members = `SELECT COUNT(*) FROM scope_hosts sh JOIN scopes sc ON sc.id = sh.scope_id WHERE sc.name = 'dmz'`
	if grCount(t, svc.db, records) != 1 || grCount(t, svc.db, members) != 1 {
		t.Fatalf("the scope's host was not imported: %d record(s), %d member(s)", grCount(t, svc.db, records), grCount(t, svc.db, members))
	}

	// The operator binds the scope; a run is waiting on it.
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
	      SELECT id, 'r-dmz', 'runner-dmz-01', 'ops@example', 'now' FROM scopes WHERE name='dmz'`)
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES ('run-waiting', 'deploy', 'bash', 'dmz', 'queued', 'seed', 'manual', 'runner', '2026-10-05T00:00:00Z')`)

	// The inventory leaves Git.
	gitRemoveFile(t, repo, "inventory/dmz.ini", "remove dmz")
	grBackdate(t, svc.db)
	exec(`UPDATE ssh_hosts SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`)
	grSync(t, svc, "sync after the removal")

	if grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE name='dmz'`) != 1 || grCount(t, svc.db, members) != 1 {
		t.Fatalf("the bound, busy scope was not held with its membership (scope rows %d, members %d)",
			grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE name='dmz'`), grCount(t, svc.db, members))
	}
	switch n := grCount(t, svc.db, records); n {
	case 0:
		// Today: the held scope's host has no record left.
	case 1:
		t.Errorf("the held scope kept its imported host record: this is fixed, and the test is to be inverted (Phase R3)")
	default:
		t.Errorf("the held scope has %d imported host records", n)
	}
}
