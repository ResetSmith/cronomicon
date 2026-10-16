package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// Present defect 67 (found by the review of 2.4.x): the owner columns 2.4.0
// added could be emptied. From 1460 a job, workflow, schedule, calendar or
// scope refuses '' (and a job NULL) on an update, and '' on an insert; a job
// written with NULL is still given its owner by jobs_born_owned. Up, down and
// up again with rows present.
func TestMigrate1460OwnerRequired(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "owner.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1450); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1450: %v", err)
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
	must(`INSERT INTO jobs (uid, name, source, run_type, synced_at) VALUES ('j1', 'nightly', 'cronomicon', 'bash', 't')`)
	must(`INSERT INTO workflows (uid, name, source, steps, synced_at) VALUES ('w1', 'flow', 'cronomicon', '[]', 't')`)
	must(`INSERT INTO schedules (uid, name, source, cron, content_hash) VALUES ('s1', 'daily', 'cronomicon', '0 2 * * *', 'h')`)
	must(`INSERT INTO calendars (source, name, created_at) VALUES ('cronomicon', 'days', 't')`)
	// What an older binary could leave: an owner emptied.
	must(`UPDATE jobs SET owner_agency = NULL WHERE uid = 'j1'`)
	must(`UPDATE workflows SET owner_agency = '' WHERE uid = 'w1'`)

	if err := m.Migrate(1460); err != nil {
		t.Fatalf("migrate to 1460: %v", err)
	}
	owner := func(q string) string {
		t.Helper()
		var o string
		if err := pool.QueryRow(q).Scan(&o); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return o
	}
	if o := owner(`SELECT owner_agency FROM jobs WHERE uid = 'j1'`); o != "global" {
		t.Errorf("the job left with no owner is %q's, want Global's", o)
	}
	if o := owner(`SELECT owner_agency FROM workflows WHERE uid = 'w1'`); o != "global" {
		t.Errorf("the workflow left with no owner is %q's, want Global's", o)
	}
	for _, q := range []string{
		`UPDATE jobs SET owner_agency = NULL WHERE uid = 'j1'`,
		`UPDATE jobs SET owner_agency = '' WHERE uid = 'j1'`,
		`UPDATE workflows SET owner_agency = '' WHERE uid = 'w1'`,
		`UPDATE schedules SET owner_agency = '' WHERE uid = 's1'`,
		`UPDATE calendars SET owner_agency = '' WHERE name = 'days'`,
		`INSERT INTO jobs (uid, name, source, run_type, synced_at, owner_agency) VALUES ('j2', 'empty', 'cronomicon', 'bash', 't', '')`,
		`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES ('sc1', 'web', 'cronomicon', 't', '')`,
	} {
		if err := exec(q); err == nil || !strings.Contains(err.Error(), "owner_required") {
			t.Errorf("%s: %v, want refused owner_required", q, err)
		}
	}
	// A job written with no owner is still given one.
	must(`INSERT INTO jobs (uid, name, source, run_type, synced_at) VALUES ('j3', 'later', 'cronomicon', 'bash', 't')`)
	if o := owner(`SELECT owner_agency FROM jobs WHERE uid = 'j3'`); o != "global" {
		t.Errorf("a job written with no owner is %q's, want Global's", o)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 1460: %v", err)
	}
	if err := exec(`UPDATE workflows SET owner_agency = '' WHERE uid = 'w1'`); err != nil {
		t.Errorf("after the down an empty owner is accepted again, as at 1450: %v", err)
	}
	must(`UPDATE workflows SET owner_agency = 'global' WHERE uid = 'w1'`)
	if err := m.Migrate(1460); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
