package db

import (
	"strings"
	"testing"
)

// TestMigrate1350ScopeOwnerFromRepo: from 1350 a new scope is given the agency
// of the repository it comes from, and a scope of an agency's repository cannot
// be given another. Existing rows are not rewritten. Then the way back.
func TestMigrate1350ScopeOwnerFromRepo(t *testing.T) {
	h := openAt(t, 1340)
	h.exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	h.exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main')`)
	agencies := func(scope string) string {
		t.Helper()
		return h.str(`SELECT COALESCE(GROUP_CONCAT(agency_id, ','), '') FROM (
			SELECT agency_id FROM scope_agencies WHERE scope_id = ? ORDER BY agency_id)`, scope)
	}
	scope := func(id, source string, repo any) {
		t.Helper()
		h.exec(`INSERT INTO scopes (id, name, source, created_at, repo_id) VALUES (?, ?, ?, 't', ?)`, id, "n-"+id, source, repo)
	}
	// Before: every scope is born Global's, an agency's repository's included,
	// and may be moved.
	scope("old-b", "git", "repo-b")
	if got := agencies("old-b"); got != "global" {
		t.Fatalf("at 1340 a scope of an agency's repository is born in %q, want global", got)
	}

	h.to(1350)

	if got := agencies("old-b"); got != "global" {
		t.Errorf("the migration rewrote an existing scope's agency: %q", got)
	}
	scope("new-b", "git", "repo-b")
	scope("new-g", "git", "global")
	scope("new-app", "cronomicon", nil)
	scope("new-odd", "cronomicon", "repo-b") // an in-app scope is no repository's, whatever the column holds
	scope("new-none", "git", "no-such-repository")
	for id, want := range map[string]string{
		"new-b": "ag-b", "new-g": "global", "new-app": "global", "new-odd": "global", "new-none": "global",
	} {
		if got := agencies(id); got != want {
			t.Errorf("the scope %s is born in %q, want %q", id, got, want)
		}
	}
	// No move for a scope of an agency's repository.
	for _, to := range []string{"global", "ag-c"} {
		if _, err := h.pool.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('new-b', ?)`, to); err == nil || !strings.Contains(err.Error(), "scope_agency_fixed") {
			t.Errorf("moving a scope of an agency's repository to %s: %v, want scope_agency_fixed", to, err)
		}
	}
	// Its own agency may be written again (a replace is a delete and an insert).
	h.exec(`DELETE FROM scope_agencies WHERE scope_id = 'new-b'`)
	h.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('new-b', 'ag-b')`)
	// The others move as before.
	h.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('new-g', 'ag-c')`)
	h.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('new-app', 'ag-b')`)
	if got := agencies("new-g"); got != "ag-c" {
		t.Errorf("Global's repository's scope after an operator assigned it: %q", got)
	}
	// And the scope goes with its row.
	h.exec(`DELETE FROM scopes WHERE id = 'new-b'`)
	if n := h.count(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'new-b'`); n != 0 {
		t.Errorf("a deleted scope left %d membership rows", n)
	}

	h.to(1340)
	scope("back-b", "git", "repo-b")
	if got := agencies("back-b"); got != "global" {
		t.Errorf("after the way back a scope is born in %q, want global", got)
	}
	h.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('back-b', 'ag-c')`)
	h.to(1350)
}
