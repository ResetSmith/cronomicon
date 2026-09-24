package scheduler_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

func openPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEnqueueRun verifies that EnqueueRunWithID inserts a queued run row.
func TestEnqueueRun(t *testing.T) {
	pool := openPool(t)

	// Seed a minimal job row (scheduler reads from jobs table).
	_, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs (uid, name, run_type, concurrency_policy, synced_at)VALUES ('uid-'||'test-job', 'test-job', 'bash', 'Allow', '2026-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	traceID, err := scheduler.EnqueueRunWithID(context.Background(), pool, scheduler.EnqueueParams{
		JobName:        "test-job",
		RunType:        "bash",
		Scope:          "prod",
		TriggerKind:    "manual",
		TriggeredBy:    "alice@example.com",
		ConcurrencyKey: "test-job",
	})
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	if traceID == "" {
		t.Fatal("expected non-empty trace ID")
	}

	var status, jobName string
	err = pool.QueryRowContext(context.Background(),
		`SELECT status, job_name FROM runs WHERE id = ?`, traceID,
	).Scan(&status, &jobName)
	if err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if jobName != "test-job" {
		t.Errorf("job_name = %q, want test-job", jobName)
	}
}

// TestEnqueueRun_ScriptSnapshot verifies the §3.3 reproducibility snapshot:
// the run row captures the job's denormalized script_ref + content_hash at
// enqueue, and a job with no script_ref leaves them NULL.
func TestEnqueueRun_ScriptSnapshot(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	// A job that references a script (script_ref + content_hash denormalized by sync).
	_, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, script_ref, content_hash, concurrency_policy, synced_at)VALUES ('uid-'||'ref-job', 'ref-job', 'bash', 'backup-db', 'sha256:abc123', 'Allow', '2026-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed ref job: %v", err)
	}
	// A legacy inline job: no script_ref, no content_hash.
	_, err = pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, concurrency_policy, synced_at)VALUES ('uid-'||'inline-job', 'inline-job', 'bash', 'Allow', '2026-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed inline job: %v", err)
	}

	refTrace, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "ref-job", RunType: "bash", TriggerKind: "manual", TriggeredBy: "a@b.c",
	})
	if err != nil {
		t.Fatalf("enqueue ref-job: %v", err)
	}
	inlineTrace, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "inline-job", RunType: "bash", TriggerKind: "manual", TriggeredBy: "a@b.c",
	})
	if err != nil {
		t.Fatalf("enqueue inline-job: %v", err)
	}

	var scriptRef, contentHash sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT script_ref, content_hash FROM runs WHERE id = ?`, refTrace).Scan(&scriptRef, &contentHash); err != nil {
		t.Fatalf("fetch ref run: %v", err)
	}
	if scriptRef.String != "backup-db" || contentHash.String != "sha256:abc123" {
		t.Errorf("ref run snapshot = (%q, %q), want (backup-db, sha256:abc123)", scriptRef.String, contentHash.String)
	}

	if err := pool.QueryRowContext(ctx,
		`SELECT script_ref, content_hash FROM runs WHERE id = ?`, inlineTrace).Scan(&scriptRef, &contentHash); err != nil {
		t.Fatalf("fetch inline run: %v", err)
	}
	if scriptRef.Valid || contentHash.Valid {
		t.Errorf("inline run should have NULL snapshot, got (%v, %v)", scriptRef, contentHash)
	}
}

// TestEnqueueRun_ScheduleNameEnvJSON verifies the firing schedule entry name
// and its env snapshot are persisted onto the runs row, and that empty values
// persist as NULL.
func TestEnqueueRun_ScheduleNameEnvJSON(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	traceID, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName:      "sched-job",
		RunType:      "bash",
		TriggerKind:  "scheduled",
		TriggeredBy:  "scheduler",
		ScheduleName: "nightly",
		EnvJSON:      `{"STAGE":"prod"}`,
		OverrideJSON: `{"env":{"FOO":"bar"}}`,
	})
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}

	var schedName, envJSON, overrideJSON sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT schedule_name, env_json, override_json FROM runs WHERE id = ?`, traceID,
	).Scan(&schedName, &envJSON, &overrideJSON); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if schedName.String != "nightly" {
		t.Errorf("schedule_name = %q, want nightly", schedName.String)
	}
	if envJSON.String != `{"STAGE":"prod"}` {
		t.Errorf("env_json = %q, want the snapshot", envJSON.String)
	}
	if overrideJSON.String != `{"env":{"FOO":"bar"}}` {
		t.Errorf("override_json = %q, want the envelope", overrideJSON.String)
	}

	// Empty schedule/env → NULL columns.
	traceID2, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "sched-job", RunType: "bash", TriggerKind: "manual", TriggeredBy: "x",
	})
	if err != nil {
		t.Fatalf("EnqueueRunWithID (empty): %v", err)
	}
	if err := pool.QueryRowContext(ctx,
		`SELECT schedule_name, env_json, override_json FROM runs WHERE id = ?`, traceID2,
	).Scan(&schedName, &envJSON, &overrideJSON); err != nil {
		t.Fatalf("fetch run 2: %v", err)
	}
	if schedName.Valid || envJSON.Valid || overrideJSON.Valid {
		t.Errorf("expected NULL schedule_name/env_json/override_json, got %v / %v / %v", schedName, envJSON, overrideJSON)
	}
}

// TestS16ForbidConflict verifies that CheckForbid returns true when an active
// run shares the same concurrency key (S16 Forbid policy).
func TestS16ForbidConflict(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	// Seed job.
	_, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, concurrency_policy, concurrency_key, synced_at)VALUES ('uid-'||'forbid-job', 'forbid-job', 'bash', 'Forbid', 'forbid-job', '2026-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// No run yet — no conflict.
	conflict, err := scheduler.CheckForbid(ctx, pool, "forbid-job")
	if err != nil {
		t.Fatalf("CheckForbid (empty): %v", err)
	}
	if conflict {
		t.Error("expected no conflict when no active runs exist")
	}

	// Enqueue a run.
	_, err = scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName:        "forbid-job",
		RunType:        "bash",
		TriggerKind:    "manual",
		TriggeredBy:    "bob@example.com",
		ConcurrencyKey: "forbid-job",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Now there's a queued run — conflict expected.
	conflict, err = scheduler.CheckForbid(ctx, pool, "forbid-job")
	if err != nil {
		t.Fatalf("CheckForbid (after enqueue): %v", err)
	}
	if !conflict {
		t.Error("expected conflict when queued run exists with same key")
	}

	// Mark the run completed — no conflict again.
	_, err = pool.ExecContext(ctx, `UPDATE runs SET status = 'success' WHERE job_name = 'forbid-job'`)
	if err != nil {
		t.Fatalf("update run: %v", err)
	}
	conflict, err = scheduler.CheckForbid(ctx, pool, "forbid-job")
	if err != nil {
		t.Fatalf("CheckForbid (after complete): %v", err)
	}
	if conflict {
		t.Error("expected no conflict after run completed")
	}
}

// TestS16AllowPolicy verifies that Allow policy never blocks.
func TestS16AllowPolicy(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	// Enqueue two runs with no concurrency key — both should succeed (Allow policy
	// uses NULL concurrency_key so the Forbid partial unique index does not fire).
	for i := range 2 {
		_, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
			JobName:     "allow-job",
			RunType:     "bash",
			TriggerKind: "manual",
			TriggeredBy: "carol@example.com",
		})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	var count int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_name = 'allow-job'`).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 queued runs, got %d", count)
	}
}

// TestSchedulerNew verifies that the scheduler can be created and started
// with no jobs in the DB (no panic, stops cleanly on context cancel).
func TestSchedulerNew(t *testing.T) {
	pool := openPool(t)
	log := discardLog()
	s := scheduler.New(pool, log, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scheduler.Start returned error: %v", err)
		}
	case <-context.Background().Done():
		t.Fatal("scheduler.Start did not return after context cancel")
	}
}

// TestEnqueueRun_CheckoutSnapshot verifies the §7/RX.2 pin: a project job
// (project_root set) snapshots the sync clone's current commit
// (git_sync_state.last_sha) and its entry (script_path) onto the run at enqueue,
// while a body-only job leaves both NULL.
func TestEnqueueRun_CheckoutSnapshot(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	if _, err := pool.ExecContext(ctx,
		`INSERT INTO git_sync_state (id, last_sha) VALUES (1, 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef')`); err != nil {
		t.Fatalf("seed git_sync_state: %v", err)
	}
	// A checkout project job: project_root set, script_path = entry.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, script_path, project_root, concurrency_policy, synced_at)VALUES ('uid-'||'proj-job', 'proj-job', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project job: %v", err)
	}
	// A body-only ansible job: no project_root.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, script, concurrency_policy, synced_at)VALUES ('uid-'||'body-job', 'body-job', 'ansible', '- hosts: all', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed body job: %v", err)
	}

	projTrace, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "proj-job", RunType: "ansible", TriggerKind: "manual", TriggeredBy: "a@b.c",
	})
	if err != nil {
		t.Fatalf("enqueue proj-job: %v", err)
	}
	bodyTrace, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "body-job", RunType: "ansible", TriggerKind: "manual", TriggeredBy: "a@b.c",
	})
	if err != nil {
		t.Fatalf("enqueue body-job: %v", err)
	}

	var sha, entry sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT checkout_sha, checkout_entry FROM runs WHERE id = ?`, projTrace).Scan(&sha, &entry); err != nil {
		t.Fatalf("fetch proj run: %v", err)
	}
	if sha.String != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" || entry.String != "scripts/proj/site.yml" {
		t.Errorf("checkout snapshot = (%q, %q), want (deadbeef…, scripts/proj/site.yml)", sha.String, entry.String)
	}

	if err := pool.QueryRowContext(ctx,
		`SELECT checkout_sha, checkout_entry FROM runs WHERE id = ?`, bodyTrace).Scan(&sha, &entry); err != nil {
		t.Fatalf("fetch body run: %v", err)
	}
	if sha.Valid || entry.Valid {
		t.Errorf("body-only run should have NULL checkout snapshot, got (%v, %v)", sha, entry)
	}
}
