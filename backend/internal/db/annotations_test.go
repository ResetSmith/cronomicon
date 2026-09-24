package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// AN-1 (the annotations plan) — the lifecycle contract of the
// annotations sidecar, which is entirely schema-level: two AFTER DELETE
// triggers, one deliberate non-event (soft delete), and a uid key that must
// keep two same-named twins apart.
//
// Each of these is a CHOICE that reads as an accident if it ever breaks. A
// cascade that stops firing leaves an orphan row the next definition of the
// same name inherits; a cascade that fires too eagerly eats the sibling's
// notes. Neither raises an error at the time.

func openAnnotationPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := Open(filepath.Join(t.TempDir(), "annotations.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func annExec(t *testing.T, pool *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, q)
	}
}

// annotate writes one annotation directly, the way the AN-2 PUT handler will.
func annotate(t *testing.T, pool *sql.DB, kind, uid, notes string) {
	t.Helper()
	annExec(t, pool, `
		INSERT INTO annotations(owner_kind, owner_uid, critical, contact, notes, updated_by, updated_at)
		VALUES (?, ?, 1, 'dba-oncall@corp.example', ?, 'operator', '2026-08-14T00:00:00Z')`,
		kind, uid, notes)
}

func annNotes(t *testing.T, pool *sql.DB, kind, uid string) string {
	t.Helper()
	var notes string
	err := pool.QueryRow(`SELECT notes FROM annotations WHERE owner_kind=? AND owner_uid=?`,
		kind, uid).Scan(&notes)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read annotation: %v", err)
	}
	return notes
}

func annCount(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM annotations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestAnnotationCascadeOnBareDelete isolates the two 1060 triggers: a bare
// DELETE on jobs / workflows — no app-layer cleanup, no sync sweep — must take
// the annotation with it, for both owner kinds.
//
// This is also the guard named in 1060's header for the rebuild hazard: the
// triggers are ON jobs/workflows, so a future 1050-style rebuild of either
// table drops them silently. If someone rebuilds and forgets to recreate, this
// test is what fails.
func TestAnnotationCascadeOnBareDelete(t *testing.T) {
	pool := openAnnotationPool(t)

	annExec(t, pool, `INSERT INTO jobs(uid, name, source, run_type, synced_at)
	                  VALUES('uid-job','doomed','git','bash','t')`)
	annExec(t, pool, `INSERT INTO workflows(uid, name, source, steps, synced_at)
	                  VALUES('uid-wf','doomed-wf','git','[]','t')`)
	annotate(t, pool, "job", "uid-job", "pages at 3am")
	annotate(t, pool, "workflow", "uid-wf", "release train")

	if got := annCount(t, pool); got != 2 {
		t.Fatalf("fixture seeded %d annotations, want 2", got)
	}

	annExec(t, pool, `DELETE FROM jobs WHERE uid='uid-job'`)
	if got := annNotes(t, pool, "job", "uid-job"); got != "" {
		t.Errorf("job annotation survived a hard delete (notes=%q) — annotations_job_delete is not firing", got)
	}

	annExec(t, pool, `DELETE FROM workflows WHERE uid='uid-wf'`)
	if got := annNotes(t, pool, "workflow", "uid-wf"); got != "" {
		t.Errorf("workflow annotation survived a hard delete (notes=%q) — annotations_workflow_delete is not firing", got)
	}
}

// TestAnnotationSurvivesSoftDeleteRestore pins the deliberate NON-event. The
// recycle bin stamps deleted_at (an UPDATE), so the AFTER DELETE triggers must
// not fire and the note must round-trip a bin-and-restore untouched.
//
// Asserted rather than inferred: "the trigger happens not to match" is exactly
// the kind of reasoning that stops being true when someone adds an
// AFTER UPDATE arm for tidiness.
func TestAnnotationSurvivesSoftDeleteRestore(t *testing.T) {
	pool := openAnnotationPool(t)

	annExec(t, pool, `INSERT INTO jobs(uid, name, source, run_type, synced_at)
	                  VALUES('uid-job','billing','amadeus','bash','t')`)
	annotate(t, pool, "job", "uid-job", "restore me")

	// Bin it.
	annExec(t, pool, `UPDATE jobs SET deleted_at='2026-08-14T00:00:00Z', deleted_by='operator' WHERE uid='uid-job'`)
	if got := annNotes(t, pool, "job", "uid-job"); got != "restore me" {
		t.Fatalf("annotation lost on SOFT delete (notes=%q) — the bin must keep it (AN-1)", got)
	}

	// Restore it.
	annExec(t, pool, `UPDATE jobs SET deleted_at=NULL, deleted_by=NULL WHERE uid='uid-job'`)
	if got := annNotes(t, pool, "job", "uid-job"); got != "restore me" {
		t.Errorf("annotation lost across bin+restore (notes=%q), want %q", got, "restore me")
	}
}

// TestAnnotationPerTwin is the R2-5 case the uid key exists for: two amadeus
// jobs may share a name, and each must carry its OWN annotation — including
// when one of them is deleted.
//
// The failure this guards is not hypothetical. It is precisely the defect
// R2F-1 fixed for reference_bindings: a name-keyed satellite serves whichever
// twin asks, and deleting either one wipes both. Nothing in this table may
// ever be queried or cascaded by name.
func TestAnnotationPerTwin(t *testing.T) {
	pool := openAnnotationPool(t)

	// Two same-named amadeus jobs in different departments — legal since 1050.
	annExec(t, pool, `INSERT INTO jobs(uid, name, source, run_type, scope, synced_at)
	                  VALUES('uid-a','backup','amadeus','bash','Finance','t')`)
	annExec(t, pool, `INSERT INTO jobs(uid, name, source, run_type, scope, synced_at)
	                  VALUES('uid-b','backup','amadeus','bash','Platform','t')`)
	annotate(t, pool, "job", "uid-a", "finance: call the DBA list")
	annotate(t, pool, "job", "uid-b", "platform: call the SRE rota")

	if got := annNotes(t, pool, "job", "uid-a"); got != "finance: call the DBA list" {
		t.Errorf("twin A reads %q — the twins are sharing a row", got)
	}
	if got := annNotes(t, pool, "job", "uid-b"); got != "platform: call the SRE rota" {
		t.Errorf("twin B reads %q — the twins are sharing a row", got)
	}

	// Deleting one twin must leave the other's annotation intact.
	annExec(t, pool, `DELETE FROM jobs WHERE uid='uid-a'`)
	if got := annNotes(t, pool, "job", "uid-a"); got != "" {
		t.Errorf("deleted twin's annotation survived (notes=%q)", got)
	}
	if got := annNotes(t, pool, "job", "uid-b"); got != "platform: call the SRE rota" {
		t.Errorf("surviving twin lost its annotation (notes=%q) — the cascade matched by name, not uid", got)
	}
}

// TestAnnotationKindsDoNotCollide: a job and a workflow that happen to carry
// the same uid string are different rows. The PK is (owner_kind, owner_uid) and
// the triggers each filter on their own kind, so a job delete must not reach
// into the workflow half. Cheap to assert, and the "one table serves both
// kinds" decision is only safe while it holds.
func TestAnnotationKindsDoNotCollide(t *testing.T) {
	pool := openAnnotationPool(t)

	annExec(t, pool, `INSERT INTO jobs(uid, name, source, run_type, synced_at)
	                  VALUES('shared-uid','thing','git','bash','t')`)
	annExec(t, pool, `INSERT INTO workflows(uid, name, source, steps, synced_at)
	                  VALUES('shared-uid','thing','git','[]','t')`)
	annotate(t, pool, "job", "shared-uid", "the job note")
	annotate(t, pool, "workflow", "shared-uid", "the workflow note")

	annExec(t, pool, `DELETE FROM jobs WHERE uid='shared-uid'`)
	if got := annNotes(t, pool, "workflow", "shared-uid"); got != "the workflow note" {
		t.Errorf("workflow annotation collateral-damaged by a job delete (notes=%q)", got)
	}
}
