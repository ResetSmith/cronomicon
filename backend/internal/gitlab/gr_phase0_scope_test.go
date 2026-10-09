package gitlab

import (
	"fmt"
	"testing"
)

// A scope's inventory file that leaves Git and comes back gives a NEW scope:
// the old row was pruned, and with it (by cascade) the agency an operator had
// assigned and the runners it was bound to. The scope that returns is born
// Global's, bound to nothing, and belongs to nobody's agency until a global
// administrator assigns it again. Nothing says so at the time. A rename does
// the same, since the name is the file's.
//
// Phase R0 read this; this test reproduces it. (A bound scope with runs still
// waiting under its name is held back from the prune; this one has none.) It
// is how a Git scope has always worked, so it is pinned as behaviour to be
// judged, not as a defect to invert: from Phase R4 a scope from an AGENCY's
// repository takes its agency from the connection and returns as that
// agency's (GR-18); a scope from Global's repository still returns as Global's.
func TestGR0_AScopeThatLeavesGitAndReturnsIsANewScope(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	const inv = "inventory/web.ini"
	body := fmt.Sprintf(grScope, "web1", "10.0.0.1")
	gitCommitFile(t, repo, remote, inv, body, "the scope")
	grSync(t, svc, "first sync")
	first := grString(t, svc.db, `SELECT id FROM scopes WHERE name='web'`)

	// An operator gives the scope to an agency and binds it to that agency's runner.
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-fin','Finance','t')`)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES(?, 'ag-fin')`, first)
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at) VALUES(?, 'r-fin', 'fin-agent', 'ops', 't')`, first)
	agencies := func(scopeID string) string {
		t.Helper()
		return grString(t, svc.db, `SELECT COALESCE(group_concat(agency_id, ','), '') FROM scope_agencies WHERE scope_id=?`, scopeID)
	}
	if got := agencies(first); got != "ag-fin" {
		t.Fatalf("the assigned scope is in %q, want ag-fin alone", got)
	}

	// The file leaves Git (a mistaken delete, or the first half of a rename).
	gitRemoveFile(t, repo, inv, "remove the scope")
	grBackdate(t, svc.db)
	grSync(t, svc, "sync after the removal")
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE name='web'`); n != 0 {
		t.Fatalf("the scope was not pruned (count %d): this test assumes an idle scope goes with its file", n)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-fin'`); n != 0 {
		t.Errorf("the runner binding outlived the scope (count %d)", n)
	}

	// The file comes back, unchanged.
	gitCommitFile(t, repo, remote, inv, body, "the scope again")
	grSync(t, svc, "sync after the return")
	second := grString(t, svc.db, `SELECT id FROM scopes WHERE name='web'`)
	if second == first {
		t.Fatalf("the returned scope kept its id: it is no longer a new scope, and this test is to be re-read")
	}
	if got := agencies(second); got != "global" {
		t.Errorf("the returned scope is in %q: today it is expected to be born Global's, the operator's assignment lost", got)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scope_runners WHERE scope_id=?`, second); n != 0 {
		t.Errorf("the returned scope is bound to %d runners: today the binding is expected to be gone", n)
	}
}
