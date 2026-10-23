package db

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1270ParksTheServersHostKeys: the two columns that held the keys
// the server captured on first connect are dropped, and what was in them is
// parked — every key, with what the server needs to turn it into an approved
// key for the local runner at its first start (the record's address and port,
// the scope an imported record belongs to, who owned a hand-written one). The
// rollback puts the columns back with those keys.
func TestMigrate1270ParksTheServersHostKeys(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "hk.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = pool.Close() }()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1260); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 1260: %v", err)
	}
	const ts = "2026-10-07T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', ?)`, ts)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-prod', 'git', ?)`, ts)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, host_key, created_at) VALUES ('h1', 'web1', '10.0.0.5', 22, 'ssh-ed25519 AAAAweb1', ?)`, ts)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, host_key, scope_id, source, owner_agency, created_at)
	      VALUES ('h2', 'db1', '10.0.0.6', 2222, 'ssh-ed25519 AAAAdb1', 'sc-fin', 'git', 'ag-fin', ?)`, ts)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_at) VALUES ('h3', 'never-connected', '10.0.0.7', 22, ?)`, ts)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, host_key, created_at) VALUES ('h4', 'blank', '10.0.0.8', 22, '   ', ?)`, ts)
	exec(`INSERT INTO bastions (id, hostname, name, address, port, host_key, created_at) VALUES ('b1', 'jump.example', 'jump', '10.0.0.9', 22, 'ssh-ed25519 AAAAjump', ?)`, ts)
	exec(`INSERT INTO bastions (id, hostname, name, address, port, created_at) VALUES ('b2', 'other.example', 'other', '10.0.0.10', 22, ?)`, ts)

	if err := m.Migrate(1270); err != nil {
		t.Fatalf("migrate to 1270: %v", err)
	}
	for _, table := range []string{"ssh_hosts", "bastions"} {
		var has int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'host_key'`, table).Scan(&has)
		if has != 0 {
			t.Errorf("%s.host_key is still there at 1270", table)
		}
	}
	type parked struct{ kind, name, hostname, address, scopeID, owner, key string }
	rows, err := pool.Query(`
		SELECT kind, name, COALESCE(hostname, ''), COALESCE(address, ''), COALESCE(scope_id, ''), owner_agency, host_key
		  FROM carried_server_host_keys WHERE carried_at IS NULL ORDER BY record_id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []parked
	for rows.Next() {
		var p parked
		if err := rows.Scan(&p.kind, &p.name, &p.hostname, &p.address, &p.scopeID, &p.owner, &p.key); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	rows.Close()
	want := []parked{
		{"bastion", "jump", "jump.example", "10.0.0.9", "", "global", "ssh-ed25519 AAAAjump"},
		{"host", "web1", "", "10.0.0.5", "", "global", "ssh-ed25519 AAAAweb1"},
		{"host", "db1", "", "10.0.0.6", "sc-fin", "ag-fin", "ssh-ed25519 AAAAdb1"},
	}
	if len(got) != len(want) {
		t.Fatalf("parked %d key(s) %+v, want %d: the records that had one, and no blank", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parked[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The records themselves are untouched.
	var hosts, bastions int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts`).Scan(&hosts)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM bastions`).Scan(&bastions)
	if hosts != 4 || bastions != 2 {
		t.Errorf("records after the migration: %d hosts, %d bastions; want 4 and 2", hosts, bastions)
	}

	// Down: the columns come back with what was parked.
	if err := m.Migrate(1260); err != nil {
		t.Fatalf("down to 1260: %v", err)
	}
	for id, wantKey := range map[string]string{"h1": "ssh-ed25519 AAAAweb1", "h2": "ssh-ed25519 AAAAdb1", "h3": ""} {
		var key string
		if err := pool.QueryRow(`SELECT COALESCE(host_key, '') FROM ssh_hosts WHERE id = ?`, id).Scan(&key); err != nil || key != wantKey {
			t.Errorf("after the down, %s.host_key = %q (%v), want %q", id, key, err, wantKey)
		}
	}
	var bkey string
	if err := pool.QueryRow(`SELECT COALESCE(host_key, '') FROM bastions WHERE id = 'b1'`).Scan(&bkey); err != nil || bkey != "ssh-ed25519 AAAAjump" {
		t.Errorf("after the down, the bastion's host_key = %q (%v)", bkey, err)
	}
	var staging int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'carried_server_host_keys'`).Scan(&staging)
	if staging != 0 {
		t.Error("the parking table survived the down")
	}
	// And up again, cleanly.
	if err := m.Migrate(1270); err != nil {
		t.Fatalf("re-up to 1270: %v", err)
	}
}
