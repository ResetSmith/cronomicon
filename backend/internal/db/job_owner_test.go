package db

import (
	"path/filepath"
	"testing"
)

// Migration 1400 (§0a, question 34): a job built in the app has an owner of
// its own. The upgrade stamps each existing row with the agency it was worked
// out to have before (its scope's one agency, Global's otherwise; a Git job's
// repository's), a row written later with no owner is born the same way, an
// owner that is given is kept, and a scope given to another agency afterwards
// does not take the job with it.
func TestJobOwnerIsStampedAndKept(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := MigrateTo(pool, 1390); err != nil {
		t.Fatalf("migrate to 1390: %v", err)
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('s-fin', 'fin-prod', 'cronomicon', 't'), ('s-two', 'legacy', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id IN ('s-fin', 's-two')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-fin', 'ag-fin'), ('s-two', 'ag-fin'), ('s-two', 'ag-tax')`)
	job := func(uid, source, scope, repo string) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, run_type, scope, repo_id, synced_at) VALUES (?, ?, ?, 'bash', NULLIF(?, ''), NULLIF(?, ''), 't')`,
			uid, uid, source, scope, repo)
	}
	// Before the upgrade.
	job("old-fin", "cronomicon", "fin-prod", "")
	job("old-two", "cronomicon", "legacy", "") // a scope several agencies share: Global's
	job("old-none", "cronomicon", "", "")
	job("old-git", "git", "fin-prod", "repo-fin")
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// After it: born with no owner, and given one.
	job("new-fin", "cronomicon", "fin-prod", "")
	job("new-git", "git", "", "repo-fin")
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, owner_agency) VALUES ('given', 'given', 'cronomicon', 'bash', 'fin-prod', 't', 'ag-tax')`)

	owner := func(uid string) string {
		t.Helper()
		var o string
		if err := pool.QueryRow(`SELECT COALESCE(owner_agency, '<null>') FROM jobs WHERE uid = ?`, uid).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	for uid, want := range map[string]string{
		"old-fin": "ag-fin", "old-two": "global", "old-none": "global", "old-git": "ag-fin",
		"new-fin": "ag-fin", "new-git": "ag-fin", "given": "ag-tax",
	} {
		if got := owner(uid); got != want {
			t.Errorf("%s is owned by %q, want %q", uid, got, want)
		}
	}

	// The scope is given to Tax: the job stays Finance's.
	exec(`DELETE FROM scope_agencies WHERE scope_id = 's-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-fin', 'ag-tax')`)
	if got := owner("old-fin"); got != "ag-fin" {
		t.Errorf("after its scope was given to Tax the job is %q's, want still Finance's", got)
	}
}
