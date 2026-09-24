package sshexec

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// newReaperService opens a migrated DB and returns a Service wired to it (no
// SSH server needed — the reaper only touches the DB).
func newReaperService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "reaper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), t.TempDir())
	// Track the async terminal-notify dispatch so it drains before the pool closes
	// (t.Cleanup is LIFO: this wg.Wait runs before the pool.Close registered above).
	wg := &sync.WaitGroup{}
	svc.WithShutdownWG(wg)
	t.Cleanup(wg.Wait)
	return svc, pool
}

// insertRun inserts a run row with the given executor/status/started_at.
func insertRun(t *testing.T, pool *sql.DB, id, executor, status, startedAt string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	var started any
	if startedAt != "" {
		started = startedAt
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, started_at, triggered_by, trigger_kind, executor, created_at)
		VALUES (?, ?, 'bash', 'sc', ?, ?, 'test', 'manual', ?, ?)`,
		id, "job-"+id, status, started, executor, now); err != nil {
		t.Fatalf("insert run %s: %v", id, err)
	}
}

func runField(t *testing.T, pool *sql.DB, id, col string) string {
	t.Helper()
	var v sql.NullString
	if err := pool.QueryRow(`SELECT `+col+` FROM runs WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("read %s.%s: %v", id, col, err)
	}
	return v.String
}

func runEndCount(t *testing.T, pool *sql.DB, traceID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind='run-end' AND trace_id = ?`, traceID).Scan(&n); err != nil {
		t.Fatalf("count run-end for %s: %v", traceID, err)
	}
	return n
}

// TestStartupSweepReconcilesOrphan: a crash-orphaned ssh run is reconciled to
// failure/executor_lost with completed_at + duration, fires the terminal seam
// exactly once, and leaves non-ssh / non-running rows untouched.
func TestStartupSweepReconcilesOrphan(t *testing.T) {
	svc, pool := newReaperService(t)

	startedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	insertRun(t, pool, "ghost", "ssh", "running", startedAt)
	insertRun(t, pool, "runner-run", "runner", "running", startedAt) // must be ignored
	insertRun(t, pool, "queued-run", "ssh", "queued", "")            // must be ignored

	svc.sweepOrphansOnStartup(context.Background())

	if got := runField(t, pool, "ghost", "status"); got != "failure" {
		t.Errorf("ghost status = %q, want failure", got)
	}
	if got := runField(t, pool, "ghost", "queued_reason"); got != "executor_lost" {
		t.Errorf("ghost queued_reason = %q, want executor_lost", got)
	}
	if got := runField(t, pool, "ghost", "completed_at"); got == "" {
		t.Error("ghost completed_at not set")
	}
	if got := runField(t, pool, "ghost", "duration_ms"); got == "" {
		t.Error("ghost duration_ms not set")
	}
	if got := runField(t, pool, "runner-run", "status"); got != "running" {
		t.Errorf("runner-run status = %q, want running (untouched — wrong executor)", got)
	}
	if got := runField(t, pool, "queued-run", "status"); got != "queued" {
		t.Errorf("queued-run status = %q, want queued (untouched — not running)", got)
	}
	// Terminal seam fired exactly once (run-end activity row).
	if c := runEndCount(t, pool, "ghost"); c != 1 {
		t.Errorf("ghost run-end activity rows = %d, want 1", c)
	}

	// Idempotency: a second sweep is a no-op (status guard) — no double-emit.
	svc.sweepOrphansOnStartup(context.Background())
	if c := runEndCount(t, pool, "ghost"); c != 1 {
		t.Errorf("after second sweep, ghost run-end rows = %d, want 1 (idempotent)", c)
	}
}

// TestPeriodicReaperRespectsStaleWindow: only runs older than the window are
// reaped; a fresh (live long-running) run survives.
func TestPeriodicReaperRespectsStaleWindow(t *testing.T) {
	svc, pool := newReaperService(t)
	now := time.Now().UTC()
	insertRun(t, pool, "old", "ssh", "running", now.Add(-48*time.Hour).Format(time.RFC3339))
	insertRun(t, pool, "fresh", "ssh", "running", now.Add(-1*time.Minute).Format(time.RFC3339))

	n := svc.reapOrphansOnce(context.Background(), 24*time.Hour)
	if n != 1 {
		t.Errorf("reaped %d, want 1", n)
	}
	if got := runField(t, pool, "old", "status"); got != "failure" {
		t.Errorf("old status = %q, want failure", got)
	}
	if got := runField(t, pool, "fresh", "status"); got != "running" {
		t.Errorf("fresh status = %q, want running (within window — live long job must survive)", got)
	}
}
