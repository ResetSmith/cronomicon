package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// driveToSuccess simulates a runner: it flips this workflow's queued/running
// child runs to success so the engine's waitForRun advances. Skipped rows
// (inserted directly by markSkipped) are left untouched.
func driveToSuccess(t *testing.T, pool *sql.DB, wfTraceID string, stop <-chan struct{}) {
	t.Helper()
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_, _ = pool.Exec(`
					UPDATE runs SET status='success', completed_at='2026-01-01T00:00:00Z'
					WHERE workflow_run_id=? AND status IN ('queued','running')`, wfTraceID)
			}
		}
	}()
}

func waitWorkflowDone(t *testing.T, pool *sql.DB, wfTraceID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status string
		if err := pool.QueryRow(`SELECT status FROM workflow_runs WHERE id=?`, wfTraceID).Scan(&status); err == nil && status != "running" {
			return status
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("workflow %s did not finish within %v", wfTraceID, timeout)
	return ""
}

func workflowEndCount(t *testing.T, pool *sql.DB, wfTraceID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind='workflow-end' AND trace_id=?`, wfTraceID).Scan(&n); err != nil {
		t.Fatalf("count workflow-end: %v", err)
	}
	return n
}

// TestTrigger_DuplicateJobName_NoHalt (PP-H8 a): the same job name in two steps
// creates two DISTINCT child runs and completes 'success' — no PK collision, no
// spurious danger-halt.
func TestTrigger_DuplicateJobName_NoHalt(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "twice")

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "dup", WorkflowID: 1, TriggeredBy: "t",
		Steps: []workflow.Step{{Type: "job", Name: "twice"}, {Type: "job", Name: "twice"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	driveToSuccess(t, pool, res.TraceID, stop)

	if got := waitWorkflowDone(t, pool, res.TraceID, 15*time.Second); got != "success" {
		t.Errorf("workflow status = %q, want success (no PK-collision halt)", got)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=?`, res.TraceID).Scan(&n)
	if n != 2 {
		t.Errorf("child run count = %d, want 2 (two distinct per-node runs)", n)
	}
	if c := workflowEndCount(t, pool, res.TraceID); c != 1 {
		t.Errorf("workflow-end count = %d, want 1", c)
	}
}

// TestTrigger_BranchNotLast_SingleWorkflowEnd (PP-H8 b): a branch that is not the
// last step emits exactly ONE workflow-end and does NOT terminate the workflow
// before the post-branch step runs.
func TestTrigger_BranchNotLast_SingleWorkflowEnd(t *testing.T) {
	pool := openPool(t)
	for _, j := range []string{"a", "c", "d", "after"} {
		seedJob(t, pool, j)
	}
	eng := workflow.New(pool, discardLog())
	steps := []workflow.Step{
		{Type: "job", Name: "a"},
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "a"},
			Pass:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "c"}}},
			Fail:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "d"}}},
		},
		{Type: "job", Name: "after"},
	}
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "branch-not-last", WorkflowID: 1, TriggeredBy: "t", Steps: steps,
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	driveToSuccess(t, pool, res.TraceID, stop)

	if got := waitWorkflowDone(t, pool, res.TraceID, 20*time.Second); got != "success" {
		t.Errorf("workflow status = %q, want success", got)
	}
	if c := workflowEndCount(t, pool, res.TraceID); c != 1 {
		t.Errorf("workflow-end count = %d, want exactly 1 (PP-H8 b)", c)
	}
	// The post-branch step must have run (workflow wasn't finalized early).
	var afterCount int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=? AND job_name='after'`, res.TraceID).Scan(&afterCount)
	if afterCount != 1 {
		t.Errorf("post-branch step 'after' run count = %d, want 1 (branch terminated workflow early)", afterCount)
	}
}

// TestTrigger_NestedBranch_SkipsAndRuns (PP-H8 c): a nested branch's executed
// job gets a real run row (non-empty PK) and BOTH skipped arms (inner + outer)
// get 'skipped' rows — the prior single-level FlattenSteps missed nested jobs.
func TestTrigger_NestedBranch_SkipsAndRuns(t *testing.T) {
	pool := openPool(t)
	for _, j := range []string{"check", "deep-deploy", "deep-rollback", "outer-rollback"} {
		seedJob(t, pool, j)
	}
	eng := workflow.New(pool, discardLog())
	steps := []workflow.Step{
		{Type: "job", Name: "check"},
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "check"},
			Pass: &workflow.Branch{Steps: []workflow.Step{
				{Type: "branch",
					Condition: &workflow.Condition{Type: "job_status", JobRef: "check"},
					Pass:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "deep-deploy"}}},
					Fail:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "deep-rollback"}}},
				},
			}},
			Fail: &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "outer-rollback"}}},
		},
	}
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "nested", WorkflowID: 1, TriggeredBy: "t", Steps: steps,
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	driveToSuccess(t, pool, res.TraceID, stop)
	waitWorkflowDone(t, pool, res.TraceID, 20*time.Second)

	runStatus := func(job string) (string, string) {
		var status, id string
		_ = pool.QueryRow(`SELECT status, id FROM runs WHERE workflow_run_id=? AND job_name=?`, res.TraceID, job).Scan(&status, &id)
		return status, id
	}
	// Executed nested job: real run row with a non-empty id.
	if st, id := runStatus("deep-deploy"); st != "success" || id == "" {
		t.Errorf("deep-deploy status=%q id=%q, want success + non-empty PK", st, id)
	}
	// Skipped inner arm — must be enumerated and marked.
	if st, _ := runStatus("deep-rollback"); st != "skipped" {
		t.Errorf("deep-rollback status=%q, want skipped (nested skip not dropped)", st)
	}
	// Skipped outer arm.
	if st, _ := runStatus("outer-rollback"); st != "skipped" {
		t.Errorf("outer-rollback status=%q, want skipped", st)
	}
}

// TestMarkSkipped_UnresolvedJob_RowPersists (PP-H8 d): a skipped-arm job absent
// from the DB (unresolved) still gets a 'skipped' row with run_type='bash' —
// previously run_type='' violated the CHECK and the row was silently dropped.
func TestMarkSkipped_UnresolvedJob_RowPersists(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "check")
	seedJob(t, pool, "deploy")
	// "ghost" intentionally NOT seeded.

	eng := workflow.New(pool, discardLog())
	steps := []workflow.Step{
		{Type: "job", Name: "check"},
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "check"},
			Pass:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "deploy"}}},
			Fail:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "ghost"}}},
		},
	}
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "unresolved-skip", WorkflowID: 1, TriggeredBy: "t", Steps: steps,
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	driveToSuccess(t, pool, res.TraceID, stop)
	waitWorkflowDone(t, pool, res.TraceID, 20*time.Second)

	var status, runType string
	if err := pool.QueryRow(`SELECT status, run_type FROM runs WHERE workflow_run_id=? AND job_name='ghost'`, res.TraceID).Scan(&status, &runType); err != nil {
		t.Fatalf("ghost skipped row missing (dropped on CHECK violation): %v", err)
	}
	if status != "skipped" || runType != "bash" {
		t.Errorf("ghost row status=%q run_type=%q, want skipped/bash", status, runType)
	}
}
