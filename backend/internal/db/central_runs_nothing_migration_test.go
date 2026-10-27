package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// 2.5.3 CA1 (migration 1670): a row a real agency owns is not changed to the
// built-in agency's. What it deliberately leaves: a row born with no owner and
// filled in as Global's (the documented limit of CA-4), a leftover of Global's
// moving out, the local runner, scopes and calendars. Up, down and up again with
// rows present.
func TestMigrate1670CentralRunsNothing(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1670); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1670: %v", err)
	}
	exec := func(q string, args ...any) error {
		_, err := pool.Exec(q, args...)
		return err
	}
	must := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	refused := func(what, q string) {
		t.Helper()
		err := exec(q)
		if err == nil || !strings.Contains(err.Error(), "central_runs_nothing") {
			t.Errorf("%s: err = %v, want central_runs_nothing", what, err)
		}
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	must(`INSERT INTO jobs (uid, name, source, run_type, owner_agency, synced_at) VALUES ('j-fin', 'nightly', 'cronomicon', 'bash', 'ag-fin', 't')`)
	must(`INSERT INTO workflows (uid, name, source, steps, owner_agency, synced_at) VALUES ('w-fin', 'flow', 'cronomicon', '[]', 'ag-fin', 't')`)
	must(`INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency) VALUES ('s-fin', 'daily', 'cronomicon', '0 2 * * *', 'h', 'ag-fin')`)

	// An agency's row is not changed to Global's.
	refused("a job", `UPDATE jobs SET owner_agency = 'global' WHERE uid = 'j-fin'`)
	refused("a workflow", `UPDATE workflows SET owner_agency = 'global' WHERE uid = 'w-fin'`)
	refused("a schedule", `UPDATE schedules SET owner_agency = 'global' WHERE uid = 's-fin'`)
	// Moving between agencies is not this trigger's business.
	must(`UPDATE jobs SET owner_agency = 'ag-tax' WHERE uid = 'j-fin'`)

	// Born with no owner: still filled in as Global's (the documented limit),
	// and a leftover of Global's may move out.
	must(`INSERT INTO jobs (uid, name, source, run_type, synced_at) VALUES ('j-born', 'born', 'cronomicon', 'bash', 't')`)
	var owner string
	if err := pool.QueryRow(`SELECT owner_agency FROM jobs WHERE uid = 'j-born'`).Scan(&owner); err != nil || owner != "global" {
		t.Fatalf("a job born with no owner is %q's (%v), want Global's", owner, err)
	}
	must(`UPDATE jobs SET owner_agency = 'ag-fin' WHERE uid = 'j-born'`)

	// Calendars are the catalogue's to hold (CA-25): not refused.
	must(`INSERT INTO calendars (source, name, created_at, owner_agency) VALUES ('cronomicon', 'days', 't', 'ag-fin')`)
	must(`UPDATE calendars SET owner_agency = 'global' WHERE name = 'days'`)

	// Down drops the triggers; up again puts them back over the same rows.
	if err := m.Migrate(1660); err != nil {
		t.Fatalf("down to 1660: %v", err)
	}
	must(`UPDATE workflows SET owner_agency = 'global' WHERE uid = 'w-fin'`)
	must(`UPDATE workflows SET owner_agency = 'ag-fin' WHERE uid = 'w-fin'`)
	if err := m.Migrate(1670); err != nil {
		t.Fatalf("up to 1670 again: %v", err)
	}
	refused("a workflow, after down and up", `UPDATE workflows SET owner_agency = 'global' WHERE uid = 'w-fin'`)
}
