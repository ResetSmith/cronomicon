package auditlog_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// R2-1 — the activity feed carries the definition's permanent uid.
//
// activity is the awkward table of the three: it has no source column, so it
// cannot always know WHICH definition a name meant. WriteActivity therefore
// resolves in a fixed order — the caller's explicit uid, then the run the event
// describes (which carries the uid since R2-1, so every run-start/run-end
// emitter is covered without being touched), then the name, and only when that
// name is unambiguous. These tests pin each rung of that ladder, including the
// one where the honest answer is NULL.
func openPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "act.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func activityUID(t *testing.T, pool *sql.DB, kind string) sql.NullString {
	t.Helper()
	var uid sql.NullString
	if err := pool.QueryRow(`SELECT job_uid FROM activity WHERE kind = ?`, kind).Scan(&uid); err != nil {
		t.Fatalf("read activity job_uid: %v", err)
	}
	return uid
}

func TestWriteActivityResolvesJobUID(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at)
	                        VALUES('nightly','git','uid-nightly','bash','t')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Rung 3: name only, and the name is unique.
	if err := auditlog.WriteActivity(ctx, pool, auditlog.ActivityParams{
		Kind: "config", Actor: "alice", JobName: "nightly",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := activityUID(t, pool, "config"); got.String != "uid-nightly" {
		t.Errorf("job_uid = %v, want uid-nightly (resolved from a unique name)", got)
	}

	// Rung 2: the event names a RUN; the uid comes off the run row. The job name
	// is deliberately absent to prove the run is what answered.
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	                        VALUES('trace-1','nightly','git','uid-nightly','bash','success','t','manual','2026-08-13T00:00:00Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := auditlog.WriteActivity(ctx, pool, auditlog.ActivityParams{
		Kind: "run-end", Outcome: "success", Actor: "alice", TraceID: "trace-1",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := activityUID(t, pool, "run-end"); got.String != "uid-nightly" {
		t.Errorf("job_uid = %v, want uid-nightly (resolved from the run row)", got)
	}

	// Rung 1: an explicit uid wins over everything, including a name that would
	// have resolved to something else.
	if err := auditlog.WriteActivity(ctx, pool, auditlog.ActivityParams{
		Kind: "gitsync", Actor: "alice", JobName: "nightly", JobUID: "uid-explicit",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := activityUID(t, pool, "gitsync"); got.String != "uid-explicit" {
		t.Errorf("job_uid = %v, want the explicitly-passed uid-explicit", got)
	}
}

// TestWriteActivityAmbiguousNameStaysNull is the case the whole ladder exists
// for: the same name in both source pools resolves to NEITHER. Guessing here
// would file one department's history under another's identity.
func TestWriteActivityAmbiguousNameStaysNull(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	for _, src := range []struct{ source, uid string }{{"git", "uid-a"}, {"amadeus", "uid-b"}} {
		if _, err := pool.Exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at)
		                        VALUES('shared', ?, ?, 'bash','t')`, src.source, src.uid); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}

	if err := auditlog.WriteActivity(ctx, pool, auditlog.ActivityParams{
		Kind: "config", Actor: "alice", JobName: "shared",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := activityUID(t, pool, "config"); got.Valid {
		t.Errorf("job_uid = %q for a name owned in both pools, want NULL", got.String)
	}

	// …but naming the run removes the ambiguity, because the run knows its source.
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	                        VALUES('trace-2','shared','amadeus','uid-b','bash','success','t','manual','2026-08-13T00:00:00Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := auditlog.WriteActivity(ctx, pool, auditlog.ActivityParams{
		Kind: "run-end", Actor: "alice", JobName: "shared", TraceID: "trace-2",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := activityUID(t, pool, "run-end"); got.String != "uid-b" {
		t.Errorf("job_uid = %v, want uid-b (the run disambiguates what the name cannot)", got)
	}
}
