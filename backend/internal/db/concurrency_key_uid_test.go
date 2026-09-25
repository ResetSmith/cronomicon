package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// R2-3 — migration 1030 rewrites in-flight concurrency keys.
//
// This is the upgrade seam, and it is the one place in AF-4b where getting it
// wrong runs a job twice rather than mislabelling something. The gate is string
// equality over runs.concurrency_key: on the release where the producers start
// composing the key from the uid, an in-flight run still holding 'git/backup'
// is invisible to a new fire computing 'uid-abc', so a Forbid job overlaps
// itself exactly once, during the upgrade, and never again — the kind of defect
// that is nearly impossible to reproduce afterwards.
func TestMigrate1030RewritesInFlightConcurrencyKeys(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "ck.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1020); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1020: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','git','uid-backup','bash','t')`)
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('shared','cronomicon','uid-shared','bash','t')`)

	// Running, with the OLD default key — must move.
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, concurrency_key, created_at)
	      VALUES('r-running','backup','git','bash','running','t','scheduled','git/backup','t')`)
	// Legacy NULL source (reads as git), old default key — must move. A DIFFERENT
	// job, because two active runs may not share a key: uq_runs_active_concurrency
	// is the gate itself, and seeding a violation of it would prove nothing.
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('sweep','git','uid-sweep','bash','t')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, concurrency_key, created_at)
	      VALUES('r-legacy','sweep','bash','queued','t','scheduled','git/sweep','t')`)
	// FINISHED with the old key — must NOT move: it is the record of what happened.
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, concurrency_key, created_at)
	      VALUES('r-done','backup','git','bash','success','t','scheduled','git/backup','t')`)
	// Active with an OPERATOR-AUTHORED key — must NOT move: jobs.concurrency_key is
	// a deliberately shared namespace and re-keying it would un-share the gate.
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, concurrency_key, created_at)
	      VALUES('r-custom','shared','cronomicon','bash','running','t','scheduled','nightly-window','t')`)
	// A parked row: promotion re-judges Forbid with this key, so it moves too.
	exec(`INSERT INTO pending_runs(id, kind, name, source, run_at, scheduled_by, created_at, status, concurrency_key)
	      VALUES('p-1','job','backup','git','t','t','t','pending','git/backup')`)

	if err := m.Migrate(1030); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1030: %v", err)
	}

	key := func(q string) string {
		t.Helper()
		var k sql.NullString
		if err := pool.QueryRow(q).Scan(&k); err != nil {
			t.Fatalf("read key: %v\n%s", err, q)
		}
		return k.String
	}
	if got := key(`SELECT concurrency_key FROM runs WHERE id='r-running'`); got != "uid-backup" {
		t.Errorf("running run key = %q, want uid-backup", got)
	}
	if got := key(`SELECT concurrency_key FROM runs WHERE id='r-legacy'`); got != "uid-sweep" {
		t.Errorf("legacy NULL-source run key = %q, want uid-sweep", got)
	}
	if got := key(`SELECT concurrency_key FROM runs WHERE id='r-done'`); got != "git/backup" {
		t.Errorf("FINISHED run key = %q, want it left at git/backup — history must not be edited", got)
	}
	if got := key(`SELECT concurrency_key FROM runs WHERE id='r-custom'`); got != "nightly-window" {
		t.Errorf("operator-authored key = %q, want nightly-window (untouched)", got)
	}
	if got := key(`SELECT concurrency_key FROM pending_runs WHERE id='p-1'`); got != "uid-backup" {
		t.Errorf("parked row key = %q, want uid-backup", got)
	}

	// And the rollback puts the in-flight rows back, so a downgraded producer can
	// still see the runs it is holding.
	if err := m.Migrate(1020); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 1020: %v", err)
	}
	if got := key(`SELECT concurrency_key FROM runs WHERE id='r-running'`); got != "git/backup" {
		t.Errorf("after rollback key = %q, want git/backup", got)
	}
	if got := key(`SELECT concurrency_key FROM runs WHERE id='r-custom'`); got != "nightly-window" {
		t.Errorf("rollback touched the operator-authored key: %q", got)
	}
}
