package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// KB — the workflow-step half of the producer conformance: a key-bound step
// whose run resolves to the ssh executor fails the STEP with the reason on the
// child row, terminal-and-recorded like the binned/disabled refusals.

func bindStepKey(t *testing.T, pool *sql.DB, job string) {
	t.Helper()
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "job", Source: "git", Name: job},
		[]runref.Binding{{Kind: runref.KindKey, Name: "deploy_key"}}, "t"); err != nil {
		t.Fatalf("bind key: %v", err)
	}
}

// seedScopedJob: scoped, so the AF unbound probe (which needs an UNSCOPED run)
// is not what refuses the step — the KB check is.
func seedScopedJob(t *testing.T, pool *sql.DB, name, executor string) {
	t.Helper()
	seedJob(t, pool, name)
	if _, err := pool.Exec(`UPDATE jobs SET executor=?, scope='tax' WHERE name=?`, executor, name); err != nil {
		t.Fatal(err)
	}
}

// waitChildRun polls for the step's child row; the engine writes it from its
// own goroutine, and a runner-executor step never reaches a terminal state
// here (there is no runner), so the terminal waiters do not apply.
func waitChildRun(t *testing.T, pool *sql.DB, jobName string) (status, reason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var qr sql.NullString
		if err := pool.QueryRow(`SELECT status, queued_reason FROM runs WHERE job_name = ? ORDER BY created_at DESC LIMIT 1`,
			jobName).Scan(&status, &qr); err == nil {
			return status, qr.String
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no child run row for step %q within 5s", jobName)
	return "", ""
}

func TestWorkflowStep_RefusesAKeyBoundJobWithNoAgent(t *testing.T) {
	pool := openPool(t)
	seedScopedJob(t, pool, "deploy", "ssh")
	bindStepKey(t, pool, "deploy")

	if status := triggerOneStep(t, pool, "deploy"); status == "success" {
		t.Error("the workflow succeeded on a step nothing could have provisioned")
	}
	status, reason := stepRun(t, pool, "deploy")
	if status != "failure" {
		t.Errorf("child run status = %q, want failure", status)
	}
	if reason != runref.ReasonKeyBindingNeedsAgent {
		t.Errorf("queued_reason = %q, want %q", reason, runref.ReasonKeyBindingNeedsAgent)
	}
}

func TestWorkflowStep_KeyBoundJobWithAnAgentIsEnqueued(t *testing.T) {
	pool := openPool(t)
	seedScopedJob(t, pool, "deploy", "ssh") // scope 'tax'; the job's executor is not read
	bindStepKey(t, pool, "deploy")
	// An agent serves the step's scope (a new scope and a new runner are both
	// Global's) — offline, even: that is an ordinary wait.
	for _, q := range []string{
		`INSERT OR IGNORE INTO scopes (id, name, source, created_at) VALUES ('sc-tax', 'tax', 'cronomicon', 't')`,
		`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('agent-1', 'agent-1', 'offline', 't', 't')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	eng := workflow.New(pool, discardLog())
	if _, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "deploy"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, reason := waitChildRun(t, pool, "deploy")
	if status == "failure" || reason == runref.ReasonKeyBindingNeedsAgent {
		t.Errorf("status/reason = %q/%q — refused although an agent can deliver the key", status, reason)
	}
}
