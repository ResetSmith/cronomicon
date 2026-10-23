package db

import (
	"strings"
	"testing"
)

// The owner's answer to question 58 (2.4.3, option A): a scope from Global's
// repository is Global's, as one from an agency's repository is that agency's.
// At 1460 a repair could put one in an agency (the reproduction); 1470 makes
// such a scope the agency's own, built in the app, with its hosts and its host
// records, marked as released from Global's repository; and refuses the repair
// from then on. Down and up again with the row present.
func TestMigrate1470GlobalScopesAreGlobals(t *testing.T) {
	pool, m := migrateTo(t, "global-scopes.db", 1460)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	must(`INSERT INTO scopes (id, name, source, created_at, repo_id, source_path, synced_at) VALUES ('sc-g', 'web', 'git', 't', 'global', 'inventory/web.yaml', 't')`)
	must(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc-g', 'h1'), ('sc-g', 'h2')`)
	must(`INSERT INTO ssh_hosts (id, hostname, source, scope_id, owner_agency, created_at) VALUES ('hr-1', 'h1', 'git', 'sc-g', 'global', 't')`)
	// At 1460 the repair goes through: the scope is FIN's, and still Global's file's.
	must(`DELETE FROM scope_agencies WHERE scope_id = 'sc-g'`)
	must(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-g', 'ag-fin')`)
	must(`INSERT INTO scopes (id, name, source, created_at, repo_id) VALUES ('sc-g2', 'db', 'git', 't', 'global')`)

	if err := m.Migrate(1470); err != nil {
		t.Fatalf("migrate to 1470: %v", err)
	}
	var source, owner string
	var repo, path, released *string
	if err := pool.QueryRow(`SELECT source, owner_agency, repo_id, source_path, released_from FROM scopes WHERE id = 'sc-g'`).
		Scan(&source, &owner, &repo, &path, &released); err != nil {
		t.Fatal(err)
	}
	if source != "cronomicon" || owner != "ag-fin" || repo != nil || path != nil || released == nil || *released != "global" {
		t.Errorf("the repaired scope is %s, %s's, repo %v, path %v, released from %v; want FIN's, built in the app, released from global",
			source, owner, repo, path, released)
	}
	var hosts int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc-g'`).Scan(&hosts)
	if hosts != 2 {
		t.Errorf("the converted scope has %d hosts, want 2", hosts)
	}
	var hrSource, hrOwner string
	_ = pool.QueryRow(`SELECT source, owner_agency FROM ssh_hosts WHERE id = 'hr-1'`).Scan(&hrSource, &hrOwner)
	if hrSource != "cronomicon" || hrOwner != "ag-fin" {
		t.Errorf("its host record is %s, %s's; want built in the app, FIN's", hrSource, hrOwner)
	}
	// From now on refused, as for an agency's repository.
	if _, err := pool.Exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-g2'`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-g2', 'ag-tax')`)
	if err == nil || !strings.Contains(err.Error(), "scope_agency_fixed") {
		t.Errorf("putting one of Global's repository's scopes in TAX at 1470: %v, want scope_agency_fixed", err)
	}
	if _, err := pool.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-g2', 'global')`); err != nil {
		t.Fatalf("putting it back in Global: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 1470: %v", err)
	}
	if err := m.Migrate(1470); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
