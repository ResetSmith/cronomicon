package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// The downs of the 2.4.0 rebuilds, with rows present (2.4.2; found by the
// review of 2.4.x: 1420 and 1430 stash and restore child tables, and were
// stepped down only on an empty database). A calendar keeps its days and a
// scope its hosts and its agency; a down that would make two agencies' names
// collide fails and leaves the table as it was.
func TestMigrateRebuildDownsKeepWhatHangsOffTheRow(t *testing.T) {
	pool, m := migrateTo(t, "downs.db", 1450)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't')`)
	must(`INSERT INTO calendars (source, name, created_at, owner_agency) VALUES ('cronomicon', 'a-days', 't', 'ag-a')`)
	must(`INSERT INTO calendar_days (calendar_owner, calendar_source, calendar_name, day) VALUES ('ag-a', 'cronomicon', 'a-days', '2026-12-25'), ('ag-a', 'cronomicon', 'a-days', '2026-12-26')`)
	must(`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES ('sc-a', 'a-web', 'cronomicon', 't', 'ag-a')`)
	must(`DELETE FROM scope_agencies WHERE scope_id = 'sc-a'`)
	must(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-a', 'ag-a')`)
	must(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc-a', 'h1'), ('sc-a', 'h2')`)

	if err := m.Migrate(1420); err != nil {
		t.Fatalf("down to 1420 (through 1430's): %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM calendar_days WHERE calendar_name = 'a-days'`); n != 2 {
		t.Errorf("after 1430's down the calendar has %d days, want 2", n)
	}
	if err := m.Migrate(1410); err != nil {
		t.Fatalf("down to 1410 (1420's): %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc-a'`); n != 2 {
		t.Errorf("after 1420's down the scope has %d hosts, want 2", n)
	}
	if n := count(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc-a' AND agency_id = 'ag-a'`); n != 1 {
		t.Errorf("after 1420's down the scope's agency is gone")
	}
	if err := m.Migrate(1460); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc-a'`); n != 2 {
		t.Errorf("up again, the scope has %d hosts, want 2", n)
	}
}

func TestMigrate1420DownRefusesTwoAgenciesScopesOfOneName(t *testing.T) {
	pool, m := migrateTo(t, "dup.db", 1420)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't'), ('ag-b', 'B', 't')`,
		`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES ('sc-a', 'web', 'cronomicon', 't', 'ag-a'), ('sc-b', 'web', 'cronomicon', 't', 'ag-b')`,
		`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc-a', 'h1'), ('sc-b', 'h2')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	err := m.Migrate(1410)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("down from 1420 with two agencies' scopes of one name: %v, want it refused", err)
	}
	var scopes, hosts int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scopes WHERE name = 'web'`).Scan(&scopes)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts`).Scan(&hosts)
	if scopes != 2 || hosts != 2 {
		t.Errorf("a refused down changed the table: %d scopes, %d hosts; want 2 and 2", scopes, hosts)
	}
}

func migrateTo(t *testing.T, file string, version uint) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	pool, err := Open(filepath.Join(t.TempDir(), file))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	return pool, m
}
