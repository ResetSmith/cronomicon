package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

//go:fix inline

//go:fix inline

// simulateRuns stands in for B4/the runner: it polls the child runs of a workflow
// and transitions each queued run to a terminal status chosen by `outcome`, keyed
// by job name and 1-based attempt number. Returns a stop func.
func simulateRuns(pool *sql.DB, wfTraceID string, outcome func(job string, attempt int) string) func() {
	done := make(chan struct{})
	go func() {
		attempts := map[string]int{}
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				rows, err := pool.Query(`SELECT id, job_name FROM runs WHERE workflow_run_id=? AND status='queued' ORDER BY created_at`, wfTraceID)
				if err != nil {
					continue
				}
				type q struct{ id, name string }
				var qs []q
				for rows.Next() {
					var x q
					if rows.Scan(&x.id, &x.name) == nil {
						qs = append(qs, x)
					}
				}
				rows.Close()
				for _, x := range qs {
					attempts[x.name]++
					st := outcome(x.name, attempts[x.name])
					_, _ = pool.Exec(`UPDATE runs SET status=?, completed_at=? WHERE id=?`, st, "2026-01-01T00:00:00Z", x.id)
				}
			}
		}
	}()
	return func() { close(done) }
}

func waitWorkflowTerminal(t *testing.T, pool *sql.DB, traceID string) (status string, cancelled int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = pool.QueryRow(`SELECT status, cancelled FROM workflow_runs WHERE id=?`, traceID).Scan(&status, &cancelled)
		if status != "running" {
			return status, cancelled
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("workflow %s did not reach terminal in time (last status %q)", traceID, status)
	return
}

// TestRunStep_RetrySucceeds verifies WB-R1: a step whose first attempt fails and
// second succeeds drives the workflow to success, with each attempt its own child
// run. The retry count comes from the per-step override (1).
func TestRunStep_RetrySucceeds(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "flaky")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "retry-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "flaky", Retries: new(1)}},
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
		t.Errorf("workflow status = %q, want success (retry should recover)", status)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=? AND job_name='flaky'`, res.TraceID).Scan(&n)
	if n != 2 {
		t.Errorf("flaky child runs = %d, want 2 (one per attempt)", n)
	}
}

// TestWalk_ContinueOnError verifies WB-R1: a failing step with continueOnError set
// does not halt the workflow — a later step still runs and the run ends success.
func TestWalk_ContinueOnError(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "flaky")
	seedJob(t, pool, "after")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "coe-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{
			{Type: "job", Name: "flaky", ContinueOnError: new(true)},
			{Type: "job", Name: "after"},
		},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, res.TraceID, func(job string, _ int) string {
		if job == "flaky" {
			return "failure"
		}
		return "success"
	})
	defer stop()

	status, _ := waitWorkflowTerminal(t, pool, res.TraceID)
	if status != "success" {
		t.Errorf("workflow status = %q, want success (continueOnError should not halt)", status)
	}
	var afterStatus string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE workflow_run_id=? AND job_name='after'`, res.TraceID).Scan(&afterStatus); err != nil {
		t.Fatalf("after step never dispatched: %v", err)
	}
	if afterStatus != "success" {
		t.Errorf("after step status = %q, want success (should run despite earlier failure)", afterStatus)
	}
}

// TestCancel_StopsDispatch verifies WB-S2: cancelling a running workflow flags it
// cancelled, marks the not-started child skipped, halts via the ctx.Done() path so
// no further step dispatches, and ends the run terminal.
func TestCancel_StopsDispatch(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "step-one")
	seedJob(t, pool, "step-two")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "cancel-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{
			{Type: "job", Name: "step-one"},
			{Type: "job", Name: "step-two"},
		},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Wait until step-one has been dispatched (queued), then cancel. No runner
	// transitions it, so it sits queued and the walk is parked in waitForRun.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=? AND job_name='step-one'`, res.TraceID).Scan(&n)
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !eng.Cancel(context.Background(), res.TraceID) {
		t.Fatal("Cancel returned false for a running workflow")
	}

	status, cancelled := waitWorkflowTerminal(t, pool, res.TraceID)
	if status == "running" {
		t.Errorf("workflow still running after cancel")
	}
	if cancelled != 1 {
		t.Errorf("cancelled flag = %d, want 1", cancelled)
	}
	// step-one (queued) should have been marked skipped by Cancel.
	var oneStatus string
	_ = pool.QueryRow(`SELECT status FROM runs WHERE workflow_run_id=? AND job_name='step-one'`, res.TraceID).Scan(&oneStatus)
	if oneStatus != "skipped" {
		t.Errorf("step-one status = %q, want skipped (not-started child)", oneStatus)
	}
	// step-two must never have been dispatched.
	var two int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=? AND job_name='step-two'`, res.TraceID).Scan(&two)
	if two != 0 {
		t.Errorf("step-two child runs = %d, want 0 (dispatch must stop on cancel)", two)
	}

	// Cancel of a finished/unknown run is a no-op.
	if eng.Cancel(context.Background(), res.TraceID) {
		t.Error("second Cancel returned true; want false (already terminal)")
	}
}
