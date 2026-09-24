package auditlog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// openTestPool gives each test its own migrated database on disk. A temp file
// rather than :memory: because the pool hands out more than one connection and
// an in-memory database is private per connection.
func openTestPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "auditlog_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// TestWriteActivityStoresTheCallersAt is the regression that matters most for
// ActivityParams.At: nearly every caller shares one timestamp between the
// activity row and the runs row it describes (completed_at, started_at, the SSH
// probe's CheckedAt). If the writer stamped its own time.Now() instead, the pair
// would drift apart — and because History sorts on `at`, a run-start could sort
// after the run-end it precedes. Both `at` and `created_at` must take the value.
func TestWriteActivityStoresTheCallersAt(t *testing.T) {
	pool := openTestPool(t)
	const at = "2026-01-02T03:04:05Z"

	if err := WriteActivity(context.Background(), pool, ActivityParams{
		At: at, Kind: "run-end", Outcome: "success", Actor: "runner:r1",
		JobName: "nightly-db-backup", TraceID: "t-1",
	}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}

	var gotAt, gotCreated string
	if err := pool.QueryRow(
		`SELECT at, created_at FROM activity WHERE trace_id = 't-1'`).
		Scan(&gotAt, &gotCreated); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotAt != at {
		t.Errorf("at = %q, want the caller's timestamp %q", gotAt, at)
	}
	if gotCreated != at {
		t.Errorf("created_at = %q, want the caller's timestamp %q", gotCreated, at)
	}
}

// TestWriteActivityDefaultsAtToNow covers the other half of the contract: a
// caller with no meaningful event time of its own (an empty At) must still get a
// well-formed current timestamp, not an empty string that would break every
// time-ordered read of the table.
func TestWriteActivityDefaultsAtToNow(t *testing.T) {
	pool := openTestPool(t)
	before := time.Now().UTC().Add(-time.Second)

	if err := WriteActivity(context.Background(), pool, ActivityParams{
		Kind: "config", Actor: "operator", Target: "runner:r1", Summary: "drain initiated",
	}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}

	var gotAt string
	if err := pool.QueryRow(`SELECT at FROM activity WHERE kind = 'config'`).Scan(&gotAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, gotAt)
	if err != nil {
		t.Fatalf("at = %q, want RFC3339: %v", gotAt, err)
	}
	if parsed.Before(before) || parsed.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("at = %q, want a timestamp around now", gotAt)
	}
}

// TestWriteChangeLogAtStoresTheCallersAt guards the backdating path the demo
// seeder depends on: its change_log rows are spread over days so the audit trail
// looks like history rather than a single instant.
func TestWriteChangeLogAtStoresTheCallersAt(t *testing.T) {
	pool := openTestPool(t)
	const at = "2025-12-25T10:00:00Z"

	if err := WriteChangeLogAt(context.Background(), pool, at,
		"alice@corp.example", "Settings", "updated", "general", "maxConcurrent 5 → 8"); err != nil {
		t.Fatalf("WriteChangeLogAt: %v", err)
	}

	var gotAt, gotCreated string
	if err := pool.QueryRow(
		`SELECT at, created_at FROM change_log WHERE target = 'general'`).
		Scan(&gotAt, &gotCreated); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotAt != at || gotCreated != at {
		t.Errorf("at/created_at = %q/%q, want %q for both", gotAt, gotCreated, at)
	}
}

// TestWriteChangeLogDefaultsAtToNow is the WriteChangeLog (no explicit time)
// counterpart — the ordinary settings/workflow callers must keep getting a
// current timestamp.
func TestWriteChangeLogDefaultsAtToNow(t *testing.T) {
	pool := openTestPool(t)
	before := time.Now().UTC().Add(-time.Second)

	if err := WriteChangeLog(context.Background(), pool,
		"bob@corp.example", "Jobs", "Paused", "vault-token-rotate", ""); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}

	var gotAt string
	if err := pool.QueryRow(
		`SELECT at FROM change_log WHERE target = 'vault-token-rotate'`).Scan(&gotAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, gotAt)
	if err != nil {
		t.Fatalf("at = %q, want RFC3339: %v", gotAt, err)
	}
	if parsed.Before(before) || parsed.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("at = %q, want a timestamp around now", gotAt)
	}
}

// TestWriteActivityInTransactionRollsBackWithIt proves what widening the writers
// to an Execer actually bought: a caller that writes its audit row inside the
// same transaction as the data it describes (the demo seeder) gets atomicity —
// if the transaction is abandoned, no orphan audit row is left behind claiming
// something happened that did not.
func TestWriteActivityInTransactionRollsBackWithIt(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := WriteActivity(ctx, tx, ActivityParams{
		At: "2026-01-02T03:04:05Z", Kind: "run-end", Outcome: "failure",
		Actor: "system", TraceID: "doomed",
	}); err != nil {
		t.Fatalf("WriteActivity in tx: %v", err)
	}
	// Visible to the transaction that wrote it...
	var inTx int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM activity WHERE trace_id = 'doomed'`).Scan(&inTx); err != nil {
		t.Fatalf("count in tx: %v", err)
	}
	if inTx != 1 {
		t.Fatalf("rows visible inside tx = %d, want 1", inTx)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// ...and gone once it is rolled back.
	var after int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM activity WHERE trace_id = 'doomed'`).Scan(&after); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if after != 0 {
		t.Errorf("rows after rollback = %d, want 0 — the write did not join the transaction", after)
	}
}
