package scheduler

import (
	"context"
	"database/sql"
	"testing"
)

// seedPinnedJobRow seeds a minimal enabled job row carrying a target_host pin
// (TG-1), mirroring seedJobRow's shape.
func seedPinnedJobRow(t *testing.T, pool *sql.DB, name, targetHost string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (uid, name, run_type, concurrency_policy, enabled, target_host, synced_at)VALUES ('uid-'||?, ?, 'bash', 'Allow', 1, ?, 't')`, name, name, targetHost); err != nil {
		t.Fatalf("seed pinned job %q: %v", name, err)
	}
}

// TestFirePersistsTargetHostPin verifies TG-1: fire()'s per-fire query now reads
// jobs.target_host and carries it onto EnqueueParams, so a scheduled cron fire
// honors a job's single-host pin the same way the manual-trigger path always
// has — before the fix, a pinned job's cron fires resolved target_host="" and
// fanned out across the whole scope.
func TestFirePersistsTargetHostPin(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedPinnedJobRow(t, pool, "pinned", "db-1.internal")

	s := New(pool, quietLog(), nil)
	s.fire("git", "pinned", "", "bash", "prod", "Allow", "", "default", "")

	var targetHost sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT target_host FROM runs WHERE job_name='pinned'`).Scan(&targetHost); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if targetHost.String != "db-1.internal" {
		t.Errorf("target_host = %q, want db-1.internal", targetHost.String)
	}
}

// TestFireNoTargetHostStaysNull is the regression guard for TG-1: a job with no
// pin (NULL jobs.target_host) must still enqueue with a NULL runs.target_host —
// not an empty string — exactly like every other optional snapshot column.
func TestFireNoTargetHostStaysNull(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "unpinned", 1) // no target_host column set ⇒ NULL

	s := New(pool, quietLog(), nil)
	s.fire("git", "unpinned", "", "bash", "prod", "Allow", "", "default", "")

	var targetHost sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT target_host FROM runs WHERE job_name='unpinned'`).Scan(&targetHost); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if targetHost.Valid {
		t.Errorf("target_host = %v, want NULL for an unpinned job", targetHost)
	}
}

// TestFireForbidSkipRecordCarriesTargetHostPin verifies that the S16 Forbid
// skip-record path (recordSkippedFire) also carries the job's target_host pin,
// so a suppressed fire's History row still displays the host the run would
// have targeted, not a blank.
func TestFireForbidSkipRecordCarriesTargetHostPin(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs (uid, name, run_type, concurrency_policy, concurrency_key, enabled, target_host, synced_at)VALUES ('uid-'||'forbid-pinned', 'forbid-pinned', 'bash', 'Forbid', 'forbid-pinned', 1, 'db-2.internal', 't')`); err != nil {
		t.Fatalf("seed forbid job: %v", err)
	}
	seedRunningRun(t, pool, "forbid-pinned") // active blocker for the Forbid key

	s := New(pool, quietLog(), nil)
	s.fire("git", "forbid-pinned", "", "bash", "prod", "Forbid", "forbid-pinned", "default", "")

	var status string
	var targetHost sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT status, target_host FROM runs WHERE job_name='forbid-pinned' AND status='skipped'`,
	).Scan(&status, &targetHost); err != nil {
		t.Fatalf("fetch skipped run: %v", err)
	}
	if targetHost.String != "db-2.internal" {
		t.Errorf("skipped run target_host = %q, want db-2.internal", targetHost.String)
	}
}
