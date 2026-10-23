package db

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1260RunnerKind pins what the kind column changes in the schema's
// own rules: every existing runner is an agent, there is at most one local
// runner, and the local runner alone may serve Global beside other agencies.
func TestMigrate1260RunnerKind(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = pool.Close() }()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1250); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 1250: %v", err)
	}
	const ts = "2026-10-07T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	serves := func(runner string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(`SELECT COALESCE(group_concat(agency_id, ','), '') FROM (
		                           SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id)`, runner).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', ?)`, ts)
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('r-agent', 'agent', 'online', ?, ?, 'ag-fin')`, ts, ts)

	if err := m.Migrate(1260); err != nil {
		t.Fatalf("migrate to 1260: %v", err)
	}
	var kind string
	if err := pool.QueryRow(`SELECT kind FROM runners WHERE id = 'r-agent'`).Scan(&kind); err != nil || kind != "agent" {
		t.Errorf("an existing runner's kind = %q (err %v), want agent", kind, err)
	}
	// The migration writes no local runner: that is the server's, at start.
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE kind = 'server'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d local runners after the migration, want none", n)
	}
	if _, err := pool.Exec(`INSERT INTO runners (id, name, kind, status, registered_at, created_at) VALUES ('r-x', 'x', 'robot', 'online', ?, ?)`, ts, ts); err == nil {
		t.Error("a runner of an unknown kind was accepted")
	}
	exec(`INSERT INTO runners (id, name, kind, status, registered_at, created_at) VALUES ('r-local', 'Local runner', 'server', 'offline', ?, ?)`, ts, ts)
	if _, err := pool.Exec(`INSERT INTO runners (id, name, kind, status, registered_at, created_at) VALUES ('r-local-2', 'second', 'server', 'offline', ?, ?)`, ts, ts); err == nil {
		t.Error("a second local runner was accepted")
	}

	// The local runner is born serving Global and may be given agencies beside
	// it, in either order; taking an agency does not take it out of Global.
	if got := serves("r-local"); got != "global" {
		t.Fatalf("the local runner is born serving %q, want global", got)
	}
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-local', 'ag-fin')`)
	if got := serves("r-local"); got != "ag-fin,global" {
		t.Errorf("after an agency is added the local runner serves %q, want both", got)
	}
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-local' AND agency_id = 'global'`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-local', 'global')`)
	if got := serves("r-local"); got != "ag-fin,global" {
		t.Errorf("Global added beside an agency: the local runner serves %q, want both", got)
	}
	// An agent is held to the old rule, both ways.
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('r-g', 'g', 'online', ?, ?)`, ts, ts)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-g', 'ag-fin')`)
	if got := serves("r-g"); got != "ag-fin" {
		t.Errorf("an agent given an agency serves %q, want it out of Global", got)
	}
	if _, err := pool.Exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-g', 'global')`); err == nil || !strings.Contains(err.Error(), "global_mixed") {
		t.Errorf("an agent was given Global beside an agency (err %v)", err)
	}

	// Down: the local runner goes with its serve list, the agents stay as they are.
	if err := m.Migrate(1250); err != nil {
		t.Fatalf("migrate down to 1250: %v", err)
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE id IN ('r-local')`).Scan(&n); err != nil || n != 0 {
		t.Errorf("down: the local runner's row is still there")
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runner_agencies WHERE runner_id = 'r-local'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("down: %d serve rows of the local runner left", n)
	}
	if got := serves("r-agent"); got != "ag-fin" {
		t.Errorf("down: the agent serves %q, want ag-fin", got)
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name = 'kind'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("down: runners.kind still present")
	}
}
