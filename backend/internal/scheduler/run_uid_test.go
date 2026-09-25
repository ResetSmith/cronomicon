package scheduler_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// R2-1 (the rbac2 plan) — every producer of a runs row stamps the
// executed job's permanent uid alongside its name.
//
// This is the guard the whole staged AF-4b migration rests on: each stage moves
// a set of edges onto the uid while names are STILL unique, so a forgotten
// producer is invisible — its rows simply carry the same name everything else
// does, and only start being unattributable on the day two agencies share a job
// name, long after the omission. Asserting the stamp at every insert site now is
// what makes that day uneventful.

// seedUIDJob inserts a job with an explicit uid (the real writers assign one at
// first sight; tests elsewhere seed without it and legitimately get NULL).
func seedUIDJob(t *testing.T, pool *sql.DB, name, source, uid string) {
	t.Helper()
	_, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs (name, source, uid, run_type, concurrency_policy, synced_at)
		VALUES (?, ?, ?, 'bash', 'Allow', '2026-01-01T00:00:00Z')
	`, name, source, uid)
	if err != nil {
		t.Fatalf("seed job %q/%q: %v", source, name, err)
	}
}

func runUID(t *testing.T, pool *sql.DB, traceID string) sql.NullString {
	t.Helper()
	var uid sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT job_uid FROM runs WHERE id = ?`, traceID).Scan(&uid); err != nil {
		t.Fatalf("read job_uid: %v", err)
	}
	return uid
}

func TestEnqueueRunStampsJobUID(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	seedUIDJob(t, pool, "backup-daily", "git", "uid-backup-git")

	traceID, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "backup-daily", RunType: "bash", TriggerKind: "manual", TriggeredBy: "alice",
	})
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	if got := runUID(t, pool, traceID); !got.Valid || got.String != "uid-backup-git" {
		t.Errorf("job_uid = %v, want uid-backup-git", got)
	}

	// EnqueueRun is the second, near-identical producer — the two INSERTs are
	// maintained in parallel and have drifted before, so both are asserted.
	if err := scheduler.EnqueueRun(ctx, pool, scheduler.EnqueueParams{
		JobName: "backup-daily", RunType: "bash", TriggerKind: "scheduled", TriggeredBy: "scheduler",
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	var n int
	if err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE job_uid = 'uid-backup-git'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("runs carrying the uid = %d, want 2 (both enqueue producers)", n)
	}
}

// TestEnqueueRunUIDResolvesPerSource pins the part that will matter after the
// final AF-4b stage: the stamp comes from the (name, source) pair, so two jobs
// sharing a name are never conflated. Same-name-across-SOURCES is already legal
// today, which is exactly why this can be tested now.
func TestEnqueueRunUIDResolvesPerSource(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	seedUIDJob(t, pool, "shared-name", "git", "uid-shared-git")
	seedUIDJob(t, pool, "shared-name", "cronomicon", "uid-shared-cronomicon")

	gitRun, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "shared-name", JobSource: "git", RunType: "bash", TriggerKind: "manual", TriggeredBy: "alice",
	})
	if err != nil {
		t.Fatalf("enqueue git run: %v", err)
	}
	amaRun, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "shared-name", JobSource: "cronomicon", RunType: "bash", TriggerKind: "manual", TriggeredBy: "alice",
	})
	if err != nil {
		t.Fatalf("enqueue cronomicon run: %v", err)
	}

	if got := runUID(t, pool, gitRun); got.String != "uid-shared-git" {
		t.Errorf("git run job_uid = %v, want uid-shared-git", got)
	}
	if got := runUID(t, pool, amaRun); got.String != "uid-shared-cronomicon" {
		t.Errorf("cronomicon run job_uid = %v, want uid-shared-cronomicon", got)
	}
}

// TestEnqueueRunUIDNullWhenJobGone documents the honest failure: a run enqueued
// for a job that no longer exists carries NULL rather than a fabricated id. The
// name column still records what was asked for.
func TestEnqueueRunUIDNullWhenJobGone(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	traceID, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "never-existed", RunType: "bash", TriggerKind: "manual", TriggeredBy: "alice",
	})
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	if got := runUID(t, pool, traceID); got.Valid {
		t.Errorf("job_uid = %q, want NULL for a job that does not exist", got.String)
	}
}
