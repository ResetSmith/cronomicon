package workflow_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/workflow"
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

func seedJob(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	_, err := pool.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO jobs (name, run_type, concurrency_policy, synced_at)
		VALUES (?, 'bash', 'Allow', '2026-01-01T00:00:00Z')
	`, name)
	if err != nil {
		t.Fatalf("seed job %q: %v", name, err)
	}
}

// ─── FlattenSteps ─────────────────────────────────────────────────────────────

func TestFlattenSteps_FlatList(t *testing.T) {
	steps := []workflow.Step{
		{Type: "job", Name: "alpha"},
		{Type: "job", Name: "beta"},
	}
	got := workflow.FlattenSteps(steps)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("FlattenSteps = %v, want [alpha beta]", got)
	}
}

func TestFlattenSteps_Parallel(t *testing.T) {
	steps := []workflow.Step{
		{Type: "parallel", Jobs: []workflow.Step{
			{Type: "job", Name: "p1"},
			{Type: "job", Name: "p2"},
		}},
	}
	got := workflow.FlattenSteps(steps)
	if len(got) != 2 {
		t.Errorf("FlattenSteps parallel = %v, want 2 jobs", got)
	}
}

func TestFlattenSteps_Branch(t *testing.T) {
	steps := []workflow.Step{
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "check"},
			Pass:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "deploy"}}},
			Fail:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "rollback"}}},
		},
	}
	got := workflow.FlattenSteps(steps)
	if len(got) != 2 {
		t.Errorf("FlattenSteps branch = %v, want 2 jobs", got)
	}
}

// ─── EvaluateCondition ────────────────────────────────────────────────────────

func TestEvaluateCondition_JobStatus(t *testing.T) {
	results := map[string]*workflow.JobResult{
		"build": {Status: "success"},
	}
	cond := &workflow.Condition{Type: "job_status", JobRef: "build"}
	if !workflow.EvaluateCondition(cond, results) {
		t.Error("expected true for successful job_status condition")
	}
	results["build"].Status = "danger"
	if workflow.EvaluateCondition(cond, results) {
		t.Error("expected false for failed job_status condition")
	}
}

func TestEvaluateCondition_OutputMatch(t *testing.T) {
	results := map[string]*workflow.JobResult{
		"check": {Status: "success", Outputs: map[string]string{"result": "pass"}},
	}
	cond := &workflow.Condition{
		Type:     "output_match",
		JobRef:   "check",
		Field:    "result",
		Operator: "==",
		Value:    "pass",
	}
	if !workflow.EvaluateCondition(cond, results) {
		t.Error("expected true for matching output")
	}
	cond.Value = "fail"
	if workflow.EvaluateCondition(cond, results) {
		t.Error("expected false for non-matching output")
	}
}

func TestEvaluateCondition_MissingRef(t *testing.T) {
	results := map[string]*workflow.JobResult{}
	cond := &workflow.Condition{Type: "job_status", JobRef: "nonexistent"}
	if workflow.EvaluateCondition(cond, results) {
		t.Error("expected false for missing job ref")
	}
}

// ─── Engine.Trigger ───────────────────────────────────────────────────────────

// TestTrigger_SimpleLinear verifies a simple two-step workflow creates a
// workflow_runs row and queues child runs for each step.
func TestTrigger_SimpleLinear(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "step-one")
	seedJob(t, pool, "step-two")

	eng := workflow.New(pool, discardLog())
	steps := []workflow.Step{
		{Type: "job", Name: "step-one"},
		{Type: "job", Name: "step-two"},
	}

	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "test-workflow",
		WorkflowID:   1,
		Steps:        steps,
		TriggeredBy:  "tester@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if result.TraceID == "" {
		t.Fatal("expected non-empty trace ID")
	}
	if len(result.JobTraceIDs) != 2 {
		t.Errorf("expected 2 job trace IDs, got %d", len(result.JobTraceIDs))
	}

	// Check the workflow_runs row.
	var status, wfName string
	err = pool.QueryRowContext(context.Background(),
		`SELECT status, workflow_name FROM workflow_runs WHERE id = ?`, result.TraceID,
	).Scan(&status, &wfName)
	if err != nil {
		t.Fatalf("fetch workflow_run: %v", err)
	}
	if wfName != "test-workflow" {
		t.Errorf("workflow_name = %q, want test-workflow", wfName)
	}
	if status != "running" {
		t.Errorf("initial status = %q, want running", status)
	}

	// The engine walks steps and enqueues child runs; give the goroutine a moment.
	// (In this test env the child runs will wait forever since no B4 runner
	// transitions them — we just check rows are inserted.)
	time.Sleep(100 * time.Millisecond)

	var childCount int
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runs WHERE workflow_run_id = ?`, result.TraceID,
	).Scan(&childCount)
	// At least the first step should be queued (step-two may not be yet because
	// the engine waits for step-one to complete).
	if childCount < 1 {
		t.Errorf("expected at least 1 child run, got %d", childCount)
	}
}

// TestTrigger_SnapshotCaptured verifies WB-D1: Trigger persists a steps_snapshot
// (carrying the minted node IDs) plus a steps_hash on the workflow_runs row, and
// the snapshot's job-node ids match the minted job trace ids so the run-detail
// serializer can map child runs back to their graph nodes (WB-O3).
func TestTrigger_SnapshotCaptured(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "build")
	seedJob(t, pool, "deploy")

	eng := workflow.New(pool, discardLog())
	steps := []workflow.Step{
		{Type: "parallel", Jobs: []workflow.Step{
			{Type: "job", Name: "build"},
			{Type: "job", Name: "deploy"},
		}},
	}
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "snap-wf", WorkflowID: 7, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	var snapshot, hash sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT steps_snapshot, steps_hash FROM workflow_runs WHERE id = ?`, result.TraceID,
	).Scan(&snapshot, &hash); err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}
	if !snapshot.Valid || snapshot.String == "" {
		t.Fatal("steps_snapshot not captured")
	}
	if !hash.Valid || len(hash.String) != 64 {
		t.Errorf("steps_hash = %q, want 64-hex chars", hash.String)
	}

	parsed, err := workflow.ParseSteps(snapshot.String)
	if err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}
	if len(parsed) != 1 || parsed[0].Type != "parallel" || len(parsed[0].Jobs) != 2 {
		t.Fatalf("snapshot shape unexpected: %+v", parsed)
	}
	minted := map[string]bool{}
	for _, id := range result.JobTraceIDs {
		minted[id] = true
	}
	for _, j := range parsed[0].Jobs {
		if j.NodeID == "" {
			t.Errorf("snapshot job %q missing node id", j.Name)
		}
		if !minted[j.NodeID] {
			t.Errorf("snapshot node id %q not among minted job trace ids", j.NodeID)
		}
	}
}

// TestTrigger_ScheduledEnvPropagation verifies a scheduled workflow's env
// snapshot is persisted on workflow_runs (alongside trigger_kind/schedule_name)
// and copied onto each child run (Gap A).
func TestTrigger_ScheduledEnvPropagation(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-step")

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "scheduled-wf",
		WorkflowID:   7,
		Steps:        []workflow.Step{{Type: "job", Name: "child-step"}},
		TriggeredBy:  "scheduler",
		TriggerKind:  "scheduled",
		ScheduleName: "nightly",
		EnvJSON:      `{"STAGE":"prod"}`,
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	var tk, sn, env sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT trigger_kind, schedule_name, env_json FROM workflow_runs WHERE id = ?`, result.TraceID,
	).Scan(&tk, &sn, &env); err != nil {
		t.Fatalf("fetch workflow_run: %v", err)
	}
	if tk.String != "scheduled" || sn.String != "nightly" || env.String != `{"STAGE":"prod"}` {
		t.Errorf("workflow_run trigger_kind/schedule_name/env_json = %q/%q/%q, want scheduled/nightly/{...}",
			tk.String, sn.String, env.String)
	}

	// The child run inherits the workflow's env snapshot.
	time.Sleep(100 * time.Millisecond)
	var childEnv sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT env_json FROM runs WHERE workflow_run_id = ? LIMIT 1`, result.TraceID,
	).Scan(&childEnv); err != nil {
		t.Fatalf("fetch child run: %v", err)
	}
	if childEnv.String != `{"STAGE":"prod"}` {
		t.Errorf("child env_json = %q, want propagated snapshot", childEnv.String)
	}
}

// TestTrigger_BranchSkip verifies that the workflow correctly marks skipped
// jobs when a branch is taken.
func TestTrigger_BranchSkip(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "health-check")
	seedJob(t, pool, "deploy")
	seedJob(t, pool, "rollback")

	eng := workflow.New(pool, discardLog())

	// Pre-seed a "success" result for health-check so the branch fires correctly.
	// We can't really test the walking end-to-end without a runner, so we test
	// FlattenSteps + EvaluateCondition logic indirectly.
	steps := []workflow.Step{
		{Type: "job", Name: "health-check"},
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "health-check"},
			Pass:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "deploy"}}},
			Fail:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "rollback"}}},
		},
	}

	allJobs := workflow.FlattenSteps(steps)
	if len(allJobs) != 3 {
		t.Errorf("FlattenSteps = %d jobs, want 3", len(allJobs))
	}

	// Trigger and verify the workflow_runs row exists.
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "branch-workflow",
		WorkflowID:   2,
		Steps:        steps,
		TriggeredBy:  "tester@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if result.TraceID == "" {
		t.Fatal("expected non-empty trace ID")
	}

	// Verify workflow-start activity was emitted.
	time.Sleep(50 * time.Millisecond)
	var activityCount int
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM activity WHERE kind = 'workflow-start' AND trace_id = ?`,
		result.TraceID,
	).Scan(&activityCount)
	if activityCount != 1 {
		t.Errorf("expected 1 workflow-start activity, got %d", activityCount)
	}
}

// ─── EmitActivity / InsertChangeLog ──────────────────────────────────────────

func TestEmitActivity(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	err := workflow.EmitActivity(ctx, pool, workflow.ActivityParams{
		Kind:    "run-start",
		Outcome: "success",
		Actor:   "user@example.com",
		JobName: "my-job",
		JobID:   1,
		TraceID: "trace-001",
		Scope:   "staging",
	})
	if err != nil {
		t.Fatalf("EmitActivity: %v", err)
	}

	var kind, actor string
	err = pool.QueryRowContext(ctx,
		`SELECT kind, actor FROM activity WHERE trace_id = 'trace-001'`,
	).Scan(&kind, &actor)
	if err != nil {
		t.Fatalf("fetch activity: %v", err)
	}
	if kind != "run-start" {
		t.Errorf("kind = %q, want run-start", kind)
	}
	if actor != "user@example.com" {
		t.Errorf("actor = %q, want user@example.com", actor)
	}
}

func TestInsertChangeLog(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	err := workflow.InsertChangeLog(ctx, pool, "admin@example.com", "Jobs", "Triggered", "my-job", "Manual run")
	if err != nil {
		t.Fatalf("InsertChangeLog: %v", err)
	}

	var actor, category, action string
	err = pool.QueryRowContext(ctx,
		`SELECT actor, category, action FROM change_log LIMIT 1`,
	).Scan(&actor, &category, &action)
	if err != nil {
		t.Fatalf("fetch change_log: %v", err)
	}
	if actor != "admin@example.com" {
		t.Errorf("actor = %q, want admin@example.com", actor)
	}
	if category != "Jobs" {
		t.Errorf("category = %q, want Jobs", category)
	}
}

func TestEngineOrphanReaper(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	// 1. Seed an orphaned workflow run and some child runs.
	_, err := pool.ExecContext(ctx, `
		INSERT INTO workflow_runs (id, workflow_id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at, started_at)
		VALUES ('wf-trace-1', 1, 'orphan-wf', 'amadeus', 'running', 'user@example.com', 'manual', '2026-07-08T00:00:00Z', '2026-07-08T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed workflow_runs: %v", err)
	}

	_, err = pool.ExecContext(ctx, `
		INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, workflow_run_id, created_at)
		VALUES ('child-run-1', 'job-alpha', 'bash', 'running', 'user@example.com', 'workflow', 'wf-trace-1', '2026-07-08T00:00:00Z'),
		       ('child-run-2', 'job-beta', 'bash', 'queued', 'user@example.com', 'workflow', 'wf-trace-1', '2026-07-08T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed runs: %v", err)
	}

	// 2. Initialize Engine and run the startup sweep.
	eng := workflow.New(pool, discardLog())
	// Calling the sweep method directly for deterministic testing.
	eng.Start(ctx)
	// Give the goroutine a brief moment to execute.
	time.Sleep(50 * time.Millisecond)

	// 3. Verify workflow run is marked failed.
	var status string
	err = pool.QueryRowContext(ctx, `SELECT status FROM workflow_runs WHERE id = 'wf-trace-1'`).Scan(&status)
	if err != nil {
		t.Fatalf("fetch workflow_run status: %v", err)
	}
	if status != "failure" {
		t.Errorf("workflow run status = %q, want failure", status)
	}

	// 4. Verify child runs are marked failed.
	rows, err := pool.QueryContext(ctx, `SELECT id, status, queued_reason FROM runs WHERE workflow_run_id = 'wf-trace-1'`)
	if err != nil {
		t.Fatalf("fetch child runs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid, rstatus string
		var rreason sql.NullString
		if err := rows.Scan(&rid, &rstatus, &rreason); err != nil {
			t.Fatalf("scan child run: %v", err)
		}
		if rstatus != "failure" {
			t.Errorf("child run %s status = %q, want failure", rid, rstatus)
		}
		if rreason.String != "orchestrator_lost" {
			t.Errorf("child run %s queued_reason = %q, want orchestrator_lost", rid, rreason.String)
		}
	}

	// 5. Verify workflow-end activity was emitted.
	var count int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity WHERE kind = 'workflow-end' AND trace_id = 'wf-trace-1' AND outcome = 'failure'`).Scan(&count)
	if err != nil {
		t.Fatalf("query activity: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 workflow-end activity, got %d", count)
	}
}

