package workflow_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// LR-42 — the workflow-step half of the producer conformance for the one
// executor: a step is written for the runner executor whatever its job's own
// `executor` says, on a bound scope and on an unbound one. (Until 2.3.0 a step
// whose job asked for ssh on a bound scope failed, `scope_requires_runner`.)

func bindStepScope(t *testing.T, pool *sql.DB, scope string) {
	t.Helper()
	if _, err := pool.Exec(
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-'||?, ?, 'cronomicon', 't')`, scope, scope); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	if _, err := pool.Exec(
		`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-'||?, 'r-dmz', 'runner-dmz-01', 't')`, scope); err != nil {
		t.Fatalf("bind scope: %v", err)
	}
}

func TestWorkflowStep_IsWrittenForTheRunnerExecutor(t *testing.T) {
	for _, tc := range []struct {
		name, jobExecutor string
		bound             bool
	}{
		{"the job says ssh, on a bound scope", "ssh", true},
		{"the job says ssh, on an unbound scope", "ssh", false},
		{"the job says runner", "runner", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := openPool(t)
			seedScopedJob(t, pool, "deploy", tc.jobExecutor) // scope 'tax'
			if tc.bound {
				bindStepScope(t, pool, "tax")
			}
			eng := workflow.New(pool, discardLog())
			if _, err := eng.Trigger(context.Background(), workflow.TriggerParams{
				WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
				Steps: []workflow.Step{{Type: "job", Name: "deploy"}},
			}); err != nil {
				t.Fatalf("Trigger: %v", err)
			}
			status, reason := waitChildRun(t, pool, "deploy")
			var executor string
			if err := pool.QueryRow(`SELECT executor FROM runs WHERE job_name='deploy'`).Scan(&executor); err != nil {
				t.Fatal(err)
			}
			if status != "queued" || executor != "runner" {
				t.Errorf("status/executor = %q/%q (reason %q), want queued/runner", status, executor, reason)
			}
		})
	}
}

// TestWorkflowStep_ARefusalIsNotRetried: a refused step fails once. A refusal is
// the engine deciding the step must not run as defined, and nothing about that
// changes between attempts — each retry used to sleep the backoff and insert
// another identical failure row. The refusal decided at enqueue is a key-bound
// shell job with no agent to deliver the key (LR-47).
func TestWorkflowStep_ARefusalIsNotRetried(t *testing.T) {
	retries, backoff := 3, 0
	pool := openPool(t)
	seedScopedJob(t, pool, "deploy", "ssh")
	bindStepKey(t, pool, "deploy")

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "deploy", Retries: &retries, BackoffSeconds: &backoff}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if status, _ := waitWorkflowTerminal(t, pool, res.TraceID); status == "success" {
		t.Error("the workflow succeeded on a refused step")
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("child runs for the refused step = %d, want 1 — a refusal must not be retried", n)
	}
}
