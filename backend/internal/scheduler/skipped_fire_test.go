package scheduler

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// countSkipped returns how many terminal 'skipped' runs exist for a concurrency key.
func countSkipped(t *testing.T, pool *sql.DB, concKey string) int {
	t.Helper()
	var n int
	if err := pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runs WHERE concurrency_key = ? AND status = 'skipped'`,
		concKey).Scan(&n); err != nil {
		t.Fatalf("count skipped: %v", err)
	}
	return n
}

// seedRunningRun inserts an active (status='running') run for a concurrency key,
// simulating the blocker that triggers a Forbid suppression.
func seedRunningRun(t *testing.T, pool *sql.DB, concKey string) {
	t.Helper()
	id := db.NewTraceID()
	// Match the RFC3339 created_at format that EnqueueRun/recordSkippedFire write,
	// so the de-dupe ordering reflects real production timestamps.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind,
			concurrency_key, created_at)
		 VALUES (?, ?, 'bash', 'running', 'scheduler', 'scheduled', ?, ?)`,
		id, concKey, concKey, now); err != nil {
		t.Fatalf("seed running run: %v", err)
	}
}

// TestRecordSkippedFire verifies V1.1-12: a Forbid-suppressed cron fire produces
// exactly one terminal 'skipped' run per blocking episode (de-duped), and a fresh
// blocking episode after the blocker clears produces a new record.
func TestRecordSkippedFire(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	const key = "j"

	// A run is already active for the key (the Forbid blocker).
	seedRunningRun(t, pool, key)

	p := EnqueueParams{
		JobName:        "j",
		RunType:        "bash",
		Scope:          "prod",
		ConcurrencyKey: key,
		ScheduleName:   "nightly",
		Executor:       "ssh",
	}

	// First suppression → one skipped record with a non-empty queued_reason.
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{Reason: "blocked by Forbid", Mode: dedupeEpisode}); err != nil {
		t.Fatalf("recordSkippedFire #1: %v", err)
	}
	if got := countSkipped(t, pool, key); got != 1 {
		t.Fatalf("after first skip: %d skipped rows, want 1", got)
	}
	var reason sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT queued_reason FROM runs WHERE concurrency_key = ? AND status = 'skipped'`,
		key).Scan(&reason); err != nil {
		t.Fatalf("fetch queued_reason: %v", err)
	}
	if !reason.Valid || reason.String == "" {
		t.Errorf("queued_reason = %q (valid=%v), want non-empty", reason.String, reason.Valid)
	}

	// De-dupe: a storm of further suppressions while the latest run is already
	// 'skipped' must NOT create additional records.
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{Reason: "blocked by Forbid", Mode: dedupeEpisode}); err != nil {
		t.Fatalf("recordSkippedFire #2: %v", err)
	}
	if got := countSkipped(t, pool, key); got != 1 {
		t.Fatalf("after de-duped skip: %d skipped rows, want 1 (storm collapsed)", got)
	}

	// New blocking episode: complete the first blocker (simulating it finished), then
	// seed a fresh running run. The latest run for the key is no longer 'skipped',
	// so the next suppression must record a SECOND skipped row.
	_, err := pool.ExecContext(ctx,
		`UPDATE runs SET status = 'success' WHERE concurrency_key = ? AND status = 'running'`, key)
	if err != nil {
		t.Fatalf("complete first blocker: %v", err)
	}
	seedRunningRun(t, pool, key)
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{Reason: "blocked by Forbid again", Mode: dedupeEpisode}); err != nil {
		t.Fatalf("recordSkippedFire #3: %v", err)
	}
	if got := countSkipped(t, pool, key); got != 2 {
		t.Fatalf("after new episode: %d skipped rows, want 2", got)
	}
}
