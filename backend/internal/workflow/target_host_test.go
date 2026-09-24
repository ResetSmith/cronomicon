package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// seedJobWithTargetHost seeds a minimal job row carrying a target_host pin
// (TG-2), mirroring seedJob's shape.
func seedJobWithTargetHost(t *testing.T, pool *sql.DB, name, targetHost string) {
	t.Helper()
	_, err := pool.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO jobs (name, run_type, concurrency_policy, target_host, synced_at)
		VALUES (?, 'bash', 'Allow', ?, '2026-01-01T00:00:00Z')
	`, name, targetHost)
	if err != nil {
		t.Fatalf("seed job %q with target_host: %v", name, err)
	}
}

// TestRunJob_TargetHostPersisted verifies TG-2: resolveJobDef reads
// jobs.target_host and runJob's child-run INSERT carries it onto the run row —
// before the fix, a workflow step for a pinned job silently fanned out across
// the job's whole scope instead of honoring the pin.
func TestRunJob_TargetHostPersisted(t *testing.T) {
	pool := openPool(t)
	seedJobWithTargetHost(t, pool, "pinned-step", "db-1.internal")

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "pin-wf",
		WorkflowID:   1,
		Steps:        []workflow.Step{{Type: "job", Name: "pinned-step"}},
		TriggeredBy:  "tester@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	var targetHost sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT target_host FROM runs WHERE workflow_run_id = ? AND job_name = 'pinned-step'`, result.TraceID,
	).Scan(&targetHost); err != nil {
		t.Fatalf("fetch child run: %v", err)
	}
	if targetHost.String != "db-1.internal" {
		t.Errorf("target_host = %q, want db-1.internal", targetHost.String)
	}
}

// TestRunJob_NoTargetHostIsNull is the regression guard for TG-2: a step whose
// job has no pin must leave the child run's target_host NULL, not an empty
// string.
func TestRunJob_NoTargetHostIsNull(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "unpinned-step")

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "nopin-wf",
		WorkflowID:   1,
		Steps:        []workflow.Step{{Type: "job", Name: "unpinned-step"}},
		TriggeredBy:  "tester@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	var targetHost sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT target_host FROM runs WHERE workflow_run_id = ? AND job_name = 'unpinned-step'`, result.TraceID,
	).Scan(&targetHost); err != nil {
		t.Fatalf("fetch child run: %v", err)
	}
	if targetHost.Valid {
		t.Errorf("target_host = %v, want NULL for an unpinned job", targetHost)
	}
}

// TestRunStep_RetryPreservesTargetHost verifies WB-R1 + TG-2 together: each
// retry attempt is a fresh child run (runStep mints a new trace ID per
// attempt), and every attempt's row still carries the step job's target_host
// pin, not just the first.
func TestRunStep_RetryPreservesTargetHost(t *testing.T) {
	pool := openPool(t)
	seedJobWithTargetHost(t, pool, "flaky-pinned", "web-3.internal")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "retry-pin-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "flaky-pinned", Retries: new(1)}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, res.TraceID, func(_ string, attempt int) string {
		if attempt == 1 {
			return "failure"
		}
		return "success"
	})
	defer stop()

	status, _ := waitWorkflowTerminal(t, pool, res.TraceID)
	if status != "success" {
		t.Fatalf("workflow status = %q, want success (retry should recover)", status)
	}

	rows, err := pool.QueryContext(context.Background(),
		`SELECT target_host FROM runs WHERE workflow_run_id = ? AND job_name = 'flaky-pinned'`, res.TraceID)
	if err != nil {
		t.Fatalf("query child runs: %v", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		n++
		var targetHost sql.NullString
		if err := rows.Scan(&targetHost); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if targetHost.String != "web-3.internal" {
			t.Errorf("attempt %d target_host = %q, want web-3.internal", n, targetHost.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 2 {
		t.Errorf("flaky-pinned child runs = %d, want 2 (one per attempt)", n)
	}
}
