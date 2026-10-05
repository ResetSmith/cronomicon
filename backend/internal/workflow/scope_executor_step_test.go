package workflow_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// SB — the workflow-step half of the producer conformance for a scope bound to
// runners: a step whose job asks for ssh fails the STEP with the reason on the
// child row, terminal-and-recorded like the key-binding refusal beside it, and
// a step whose job expresses no executor is sent to the bound runners.

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

func TestWorkflowStep_RefusesSSHOnABoundScope(t *testing.T) {
	pool := openPool(t)
	seedScopedJob(t, pool, "deploy", "ssh") // scope 'tax'
	bindStepScope(t, pool, "tax")

	if status := triggerOneStep(t, pool, "deploy"); status == "success" {
		t.Error("the workflow succeeded on a step that would have run from the server on a bound scope")
	}
	status, reason := stepRun(t, pool, "deploy")
	if status != "failure" {
		t.Errorf("child run status = %q, want failure", status)
	}
	if reason != execspec.ReasonScopeRequiresRunner {
		t.Errorf("queued_reason = %q, want %q", reason, execspec.ReasonScopeRequiresRunner)
	}
}

func TestWorkflowStep_OnABoundScopeRunsOnItsRunners(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "deploy") // bash, no executor of its own
	if _, err := pool.Exec(`UPDATE jobs SET scope='tax' WHERE name='deploy'`); err != nil {
		t.Fatal(err)
	}
	bindStepScope(t, pool, "tax")

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
		t.Errorf("status/executor = %q/%q (reason %q), want queued/runner — a shell step on a bound scope must not default to ssh",
			status, executor, reason)
	}
}
