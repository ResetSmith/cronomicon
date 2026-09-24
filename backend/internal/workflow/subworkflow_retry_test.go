package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// SW — a sub-workflow step's retries must actually re-run the child.
//
// The inheritance table says `retries` are the STEP's, applied to the child run
// as a unit: a failed child is re-triggered whole. But runWorkflowStep only ever
// triggered the child once, so `{type: workflow, retries: 3}` validated clean,
// rendered in the canvas, and quietly ran exactly one attempt. Nothing failed
// loudly — the workflow just never recovered from a transient child failure, and
// the retry count in the YAML was decoration. These tests count the child's
// workflow_runs rows, which is the only evidence an attempt happened at all.

// simulateAllAttempts drains every queued run in the database, whichever
// workflow run owns it, choosing an outcome per job name and 1-based attempt
// number. It is simulateAllRuns (parent + child, unlike simulateRuns) crossed
// with simulateRuns' attempt counter — which is exactly what a retrying
// sub-workflow needs: its child's jobs belong to the CHILD's run, and each retry
// re-runs them.
func simulateAllAttempts(pool *sql.DB, outcome func(job string, attempt int) string) func() {
	done := make(chan struct{})
	go func() {
		attempts := map[string]int{}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				rows, err := pool.Query(`SELECT id, job_name FROM runs WHERE status='queued' ORDER BY created_at`)
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
					_, _ = pool.Exec(`UPDATE runs SET status=?, completed_at=? WHERE id=?`,
						outcome(x.name, attempts[x.name]), "2026-01-01T00:00:00Z", x.id)
				}
			}
		}
	}()
	return func() { close(done) }
}

// waitWorkflowTerminalWithin is waitWorkflowTerminal with an explicit budget.
// A sub-workflow retry costs TWO DB-poll ticks per attempt — the child's own
// wait on its step run, then the parent's wait on the child run — so a
// three-attempt test does not fit the shared helper's 15s.
func waitWorkflowTerminalWithin(t *testing.T, pool *sql.DB, traceID string, budget time.Duration) (status string, cancelled int) {
	t.Helper()
	deadline := time.Now().Add(budget)
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

// childRunCount counts the workflow_runs rows a named child produced. Each
// attempt is a FRESH run — the same reason runStep mints a new trace id per job
// attempt — so this is the attempt count.
func childRunCount(t *testing.T, pool *sql.DB, name string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_name=?`, name).Scan(&n)
	return n
}

// TestSubWorkflowStep_RetriesRerunTheChild is the regression: retries=2 on a
// workflow step whose child always fails must produce three child runs, not one.
func TestSubWorkflowStep_RetriesRerunTheChild(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "run-child", Workflow: "child", Retries: new(2)},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllAttempts(pool, func(string, int) string { return "failure" })
	defer stop()

	if status, _ := waitWorkflowTerminalWithin(t, pool, res.TraceID, 60*time.Second); status == "success" {
		t.Error("the parent succeeded despite every child attempt failing")
	}
	if n := childRunCount(t, pool, "child"); n != 3 {
		t.Errorf("child workflow runs = %d, want 3 (retries=2 ⇒ one attempt + two retries); "+
			"1 means the step's retries never reached the child", n)
	}
}

// TestSubWorkflowStep_RetrySucceedsOnALaterAttempt: a child that fails once and
// then succeeds must leave the step non-danger, so the walk carries on into the
// next step rather than halting.
func TestSubWorkflowStep_RetrySucceedsOnALaterAttempt(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedJob(t, pool, "after")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "run-child", Workflow: "child", Retries: new(2)},
		{Type: "job", Name: "after"},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllAttempts(pool, func(job string, attempt int) string {
		if job == "child-job" && attempt == 1 {
			return "failure"
		}
		return "success"
	})
	defer stop()

	if status, _ := waitWorkflowTerminalWithin(t, pool, res.TraceID, 60*time.Second); status != "success" {
		t.Errorf("parent status = %q, want success — the retry should have recovered the step", status)
	}
	// Exactly two: the retry loop stops the moment an attempt is no longer danger.
	if n := childRunCount(t, pool, "child"); n != 2 {
		t.Errorf("child workflow runs = %d, want 2 (fail then succeed)", n)
	}
	var afterStatus string
	if err := pool.QueryRow(
		`SELECT status FROM runs WHERE workflow_run_id=? AND job_name='after'`, res.TraceID).Scan(&afterStatus); err != nil {
		t.Fatalf("the step after the recovered sub-workflow never dispatched: %v", err)
	}
}

// TestSubWorkflowStep_NoRetriesRunsOnce pins the other end of the range: the
// retry wrapper must not turn an unconfigured step into a re-running one.
func TestSubWorkflowStep_NoRetriesRunsOnce(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "workflow", Name: "run-child", Workflow: "child"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllAttempts(pool, func(string, int) string { return "failure" })
	defer stop()

	if status, _ := waitWorkflowTerminalWithin(t, pool, res.TraceID, 30*time.Second); status == "success" {
		t.Error("the parent succeeded despite its sub-workflow failing")
	}
	if n := childRunCount(t, pool, "child"); n != 1 {
		t.Errorf("child workflow runs = %d, want 1 (no retries configured)", n)
	}
}

// TestSubWorkflowStep_CancelStopsRetries: the retry loop honors cancellation at
// both seams (the ctx.Err() check and the backoff timer's select), so a
// cancelled parent stops re-triggering its child instead of grinding through the
// remaining attempts nobody is waiting for. Without that, cancel would take
// retries × (backoff + a whole child run) to take effect.
func TestSubWorkflowStep_CancelStopsRetries(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "workflow", Name: "run-child", Workflow: "child",
			Retries: new(5), BackoffSeconds: new(3)}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllAttempts(pool, func(string, int) string { return "failure" })
	defer stop()

	// Wait until the first child attempt exists, then cancel the tree.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && childRunCount(t, pool, "child") == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if childRunCount(t, pool, "child") == 0 {
		t.Fatal("no child run was ever triggered")
	}

	start := time.Now()
	if !eng.CancelTree(context.Background(), res.TraceID) {
		t.Fatal("CancelTree reported nothing cancelled")
	}
	status, cancelled := waitWorkflowTerminalWithin(t, pool, res.TraceID, 30*time.Second)
	elapsed := time.Since(start)

	if status == "running" {
		t.Error("the parent is still running after cancel")
	}
	if cancelled != 1 {
		t.Errorf("cancelled flag = %d, want 1", cancelled)
	}
	// Six attempts with a 3s backoff cannot finish in under ~27s; anything near
	// that means the cancel was ignored until the retries ran out.
	if elapsed > 12*time.Second {
		t.Errorf("parent took %s to go terminal after cancel — the retry loop is not honoring ctx", elapsed)
	}
	if n := childRunCount(t, pool, "child"); n > 2 {
		t.Errorf("child attempts after cancel = %d, want ≤2 — retries kept re-triggering a cancelled step", n)
	}
}
