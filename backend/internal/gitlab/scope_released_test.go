package gitlab

import (
	"fmt"
	"testing"
)

// The owner's answer to question 58 (2.4.3, option A): a scope of Global's
// repository that a repair gave to an agency before 2.4.3 is that agency's own
// (migration 1470, released_from). Global's file of the name is no longer any
// scope's: its sync makes no second scope of the name for Global, and says so
// on the file, until the agency deletes its scope, when the file is Global's
// scope again.
//
// The built-in agency has no repository since 2.5.3 (CA-6), so nothing syncs
// a file of Global's. The rule is the sync's for any repository (a scope
// released_from the repository syncing), and is held here on an agency's
// (ag-a's repo-a, newSyncFixture): its file of a scope released from it is no
// scope of ag-a's until FIN deletes the scope.
func TestRV_AScopeGivenAwayIsNotMadeAgainFromItsFile(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	exec(`INSERT INTO scopes (id, name, source, created_at, owner_agency, released_from) VALUES ('sc-r', 'web', 'cronomicon', 't', 'ag-fin', ?)`, fixtureRepo)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-r'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-r', 'ag-fin')`)

	grCommitFiles(t, repoA, remoteA, map[string]string{"inventory/web.ini": fmt.Sprintf(grScope, "web1", "10.0.0.1")}, "the file of web")
	grSync(t, a, "the repository with the file of a released scope")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scopes WHERE name = 'web' AND owner_agency = 'ag-a'`); n != 0 {
		t.Errorf("the repository's sync made %d scope(s) web for its agency, want none", n)
	}
	if rows := grProblems(t, a, fixtureRepo); !grHas(rows, "inventory/web.ini", "was given to an agency") {
		t.Errorf("the repository's problems do not say the file's scope was given away: %v", rows)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc-r'`); n != 0 {
		t.Errorf("the repository's sync wrote %d host(s) into FIN's scope", n)
	}

	exec(`DELETE FROM scopes WHERE id = 'sc-r'`)
	grSync(t, a, "the repository again, the agency's scope gone")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scopes WHERE name = 'web' AND owner_agency = 'ag-a' AND source = 'git'`); n != 1 {
		t.Errorf("after FIN deleted its scope, the repository's file made %d scope(s) web, want 1", n)
	}
}
