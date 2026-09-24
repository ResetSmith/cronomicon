package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// R2-1 — the workflow engine is the second run producer, and it writes to BOTH
// history tables: the workflow_runs row it owns and the child runs rows it
// enqueues per step. Each must carry its definition's permanent uid, or a
// workflow's history becomes unattributable exactly when the jobs' history is
// not (the asymmetry that would make the final AF-4b stage unsafe).

func seedUIDJob(t *testing.T, pool *sql.DB, name, uid string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs (name, source, uid, run_type, concurrency_policy, synced_at)
		VALUES (?, 'git', ?, 'bash', 'Allow', '2026-01-01T00:00:00Z')
	`, name, uid); err != nil {
		t.Fatalf("seed job %q: %v", name, err)
	}
}

func TestTriggerStampsWorkflowAndChildUIDs(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	seedUIDJob(t, pool, "step-one", "uid-step-one")
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO workflows (name, source, uid, steps, synced_at)
		VALUES ('nightly-pipeline', 'git', 'uid-pipeline', '[]', '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(ctx, workflow.TriggerParams{
		WorkflowName: "nightly-pipeline",
		WorkflowID:   1,
		Steps:        []workflow.Step{{Type: "job", Name: "step-one"}},
		TriggeredBy:  "tester@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	var wfUID sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT workflow_uid FROM workflow_runs WHERE id = ?`, result.TraceID).Scan(&wfUID); err != nil {
		t.Fatalf("read workflow_uid: %v", err)
	}
	if wfUID.String != "uid-pipeline" {
		t.Errorf("workflow_runs.workflow_uid = %v, want uid-pipeline", wfUID)
	}

	// The step walk runs in a goroutine (see TestTrigger_SimpleLinear).
	time.Sleep(200 * time.Millisecond)

	var childUID sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT job_uid FROM runs WHERE workflow_run_id = ? AND job_name = 'step-one'`,
		result.TraceID).Scan(&childUID); err != nil {
		t.Fatalf("read child run job_uid: %v", err)
	}
	if childUID.String != "uid-step-one" {
		t.Errorf("child run job_uid = %v, want uid-step-one", childUID)
	}
}
