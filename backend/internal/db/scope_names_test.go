package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// Migration 1420 (§0a, IN-5; Phase I2): a scope's name is unique within its
// agency, not across the installation. The table is rebuilt for it, and the
// rows of the eight tables that hang off it must come through: DROP TABLE
// fires their ON DELETE CASCADE while foreign keys are on (the 830 lesson).
func TestScopeNamesArePerAgencyAndTheRebuildKeepsEverything(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "scopes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := MigrateTo(pool, 1410); err != nil {
		t.Fatalf("migrate to 1410: %v", err)
	}
	exec := func(q string, a ...any) error {
		_, err := pool.Exec(q, a...)
		return err
	}
	must := func(q string, a ...any) {
		t.Helper()
		if err := exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	must(`INSERT INTO scopes (id, name, source, created_at) VALUES ('s-web', 'web', 'cronomicon', 't')`)
	must(`DELETE FROM scope_agencies WHERE scope_id = 's-web'`)
	must(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-web', 'ag-fin')`)
	must(`INSERT INTO scope_hosts (scope_id, host) VALUES ('s-web', 'web01'), ('s-web', 'web02')`)
	must(`INSERT INTO scope_groups (scope_id, name) VALUES ('s-web', 'front'), ('s-web', 'all')`)
	must(`INSERT INTO scope_group_hosts (scope_id, group_name, host) VALUES ('s-web', 'front', 'web01')`)
	must(`INSERT INTO scope_group_children (scope_id, parent, child) VALUES ('s-web', 'all', 'front')`)
	must(`INSERT INTO scope_host_vars (scope_id, host, key, value) VALUES ('s-web', 'web01', 'port', '8080')`)
	must(`INSERT INTO scope_group_vars (scope_id, group_name, key, value) VALUES ('s-web', 'front', 'tier', 'edge')`)
	must(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('s-web', 'r1', 'r1', 't')`)
	counts := func() string {
		t.Helper()
		var parts []string
		for _, tbl := range []string{"scope_hosts", "scope_groups", "scope_group_hosts", "scope_group_children",
			"scope_host_vars", "scope_group_vars", "scope_agencies", "scope_runners"} {
			var n int
			if err := pool.QueryRow(`SELECT COUNT(*) FROM ` + tbl + ` WHERE scope_id = 's-web'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, tbl+"="+string(rune('0'+n)))
		}
		return strings.Join(parts, " ")
	}
	before := counts()
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if after := counts(); after != before {
		t.Fatalf("the rebuild lost rows: before %s, after %s", before, after)
	}

	// Tax may have a scope `web` of its own; Finance may not have two.
	if err := exec(`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES ('s-web-tax', 'web', 'cronomicon', 't', 'ag-tax')`); err != nil {
		t.Fatalf("Tax's own `web` was refused: %v", err)
	}
	var born string
	if err := pool.QueryRow(`SELECT group_concat(agency_id) FROM scope_agencies WHERE scope_id = 's-web-tax'`).Scan(&born); err != nil || born != "ag-tax" {
		t.Errorf("Tax's scope was born into %q (%v), want ag-tax", born, err)
	}
	if err := exec(`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES ('s-web-2', 'web', 'cronomicon', 't', 'ag-fin')`); err == nil {
		t.Error("a second `web` in Finance was accepted")
	}
	// A move into an agency that has the name is refused too, by the key.
	if err := exec(`DELETE FROM scope_agencies WHERE scope_id = 's-web-tax'`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-web-tax', 'ag-fin')`); err == nil {
		t.Error("moving Tax's `web` into Finance, which has one, was accepted")
	}
}
