package db

import (
	"path/filepath"
	"testing"
)

// Migration 1430 (§0a, IN-5; Phase I2; questions 30 and 35): a calendar's name
// is unique within its agency, the days come through the rebuild, and each
// agency gets its copies — of every global calendar of Global's, and of each
// calendar its bindings name and it has none of — under the same name, with
// the days, so that no binding changes.
func TestCalendarNamesArePerAgencyAndTheUpgradeCopies(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "calendars.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := MigrateTo(pool, 1420); err != nil {
		t.Fatalf("migrate to 1420: %v", err)
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
	count := func(q string, a ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(q, a...).Scan(&n); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		return n
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	must(`INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES
	      ('cronomicon', 'freeze',   1, 0, 'root', 't', 'global'),
	      ('cronomicon', 'holidays', 0, 1, 'root', 't', 'global'),
	      ('cronomicon', 'nobody',   0, 0, 'root', 't', 'global'),
	      ('cronomicon', 'own',      0, 0, 'fin',  't', 'ag-fin')`)
	must(`INSERT INTO calendar_days (calendar_source, calendar_name, day, label) VALUES
	      ('cronomicon', 'freeze', '2026-12-24', 'eve'), ('cronomicon', 'freeze', '2026-12-25', 'day'),
	      ('cronomicon', 'holidays', '2026-07-04', 'fourth'),
	      ('cronomicon', 'nobody', '2026-01-01', 'nyd'),
	      ('cronomicon', 'own', '2026-03-01', 'fin')`)
	// Finance's job names Global's holidays; Tax's schedule names it too.
	must(`INSERT INTO jobs (uid, name, source, run_type, synced_at, owner_agency) VALUES ('j-fin', 'fin-job', 'cronomicon', 'bash', 't', 'ag-fin')`)
	must(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, skip_calendars)
	      VALUES ('cronomicon', 'job', 'fin-job', 'nightly', '0 2 * * *', 0, 'j-fin', '["holidays"]')`)
	must(`INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency, only_calendars) VALUES ('s-tax', 'weekly', 'cronomicon', '0 3 * * 1', 'h', 'ag-tax', '["holidays"]')`)

	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The rebuild kept every day.
	if n := count(`SELECT COUNT(*) FROM calendar_days WHERE calendar_owner = 'global'`); n != 4 {
		t.Errorf("Global's days after the rebuild = %d, want 4", n)
	}
	// The copies: each agency has Global's global calendar (question 35) and
	// the calendar its binding names; Finance keeps its own; nobody copied
	// what nothing names.
	for _, c := range []struct {
		agency, name string
		global, days int
	}{
		{"ag-fin", "freeze", 1, 2}, {"ag-fin", "holidays", 0, 1}, {"ag-fin", "own", 0, 1},
		{"ag-tax", "freeze", 1, 2}, {"ag-tax", "holidays", 0, 1},
	} {
		var global, recordSupp int
		var createdBy string
		if err := pool.QueryRow(`SELECT global, record_suppressed, COALESCE(created_by, '') FROM calendars WHERE owner_agency = ? AND name = ?`,
			c.agency, c.name).Scan(&global, &recordSupp, &createdBy); err != nil {
			t.Errorf("%s has no calendar %s: %v", c.agency, c.name, err)
			continue
		}
		if global != c.global {
			t.Errorf("%s's %s: global = %d, want %d", c.agency, c.name, global, c.global)
		}
		if c.name == "holidays" && recordSupp != 1 {
			t.Errorf("%s's holidays did not keep record_suppressed", c.agency)
		}
		if n := count(`SELECT COUNT(*) FROM calendar_days WHERE calendar_owner = ? AND calendar_name = ?`, c.agency, c.name); n != c.days {
			t.Errorf("%s's %s has %d day(s), want %d", c.agency, c.name, n, c.days)
		}
	}
	if n := count(`SELECT COUNT(*) FROM calendars WHERE name = 'nobody'`); n != 1 {
		t.Errorf("a calendar nothing names was copied: %d rows", n)
	}
	if n := count(`SELECT COUNT(*) FROM calendars WHERE owner_agency = 'ag-tax' AND name = 'own'`); n != 0 {
		t.Errorf("Finance's own calendar was copied to Tax")
	}
	if n := count(`SELECT COUNT(*) FROM calendars WHERE created_by LIKE 'upgrade 2.4.0%'`); n != 4 {
		t.Errorf("%d copies record where they came from, want 4", n)
	}

	// A name once per agency, and a second agency's is its own: deleting
	// Tax's holidays takes Tax's days and nobody else's.
	if err := exec(`INSERT INTO calendars (source, name, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 't', 'ag-fin')`); err == nil {
		t.Error("a second holidays in Finance was accepted")
	}
	if err := exec(`INSERT INTO calendars (source, name, created_at, owner_agency) VALUES ('git', 'holidays', 't', 'ag-fin')`); err == nil {
		t.Error("a second holidays in Finance, from Git, was accepted")
	}
	must(`DELETE FROM calendars WHERE owner_agency = 'ag-tax' AND name = 'holidays'`)
	if n := count(`SELECT COUNT(*) FROM calendar_days WHERE calendar_name = 'holidays'`); n != 2 {
		t.Errorf("days of holidays after deleting Tax's = %d, want Global's and Finance's (2)", n)
	}
}
