package workflow_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Present defect 44, the workflow-step half: a step whose job is a project,
// with no registered agent that allows checkout to wait for, FAILS on its
// child row with the reason. Left queued for an agent that does not exist it
// would hold the workflow for its whole timeout, and the installation's cap
// with it.

func seedProjectStep(t *testing.T, pool *sql.DB, job string) {
	t.Helper()
	seedJob(t, pool, job)
	for _, q := range []string{
		`UPDATE git_repos SET last_sha = '1111111111111111111111111111111111111111' WHERE id = 'global'`,
		`UPDATE jobs SET run_type = 'ansible', command = NULL, script = NULL, script_path = 'scripts/proj/site.yml', project_root = 'scripts/proj' WHERE name = '` + job + `'`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
}

func TestWorkflowStep_RefusesAProjectJobWithNoCheckoutAgent(t *testing.T) {
	pool := openPool(t)
	seedProjectStep(t, pool, "proj")

	if status := triggerOneStep(t, pool, "proj"); status == "success" {
		t.Error("the workflow succeeded on a step that no agent could take")
	}
	status, reason := stepRun(t, pool, "proj")
	if status != "failure" || reason != runref.ReasonCheckoutNeedsAgent {
		t.Errorf("the step's run is %q with reason %q; want failure, %q", status, reason, runref.ReasonCheckoutNeedsAgent)
	}
}

// The control: with an agent that allows checkout registered, the step is
// enqueued and waits for it.
func TestWorkflowStep_AProjectJobWithACheckoutAgentIsEnqueued(t *testing.T) {
	pool := openPool(t)
	seedProjectStep(t, pool, "proj")
	for _, q := range []string{
		`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
		 VALUES ('checkout-agent', 'checkout-agent', 'agent', 'offline', 'Linux', '["ansible","checkout"]', 0, 5, '1.0', 't', 't')`,
		`INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES ('checkout-agent', 'global')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	eng := workflow.New(pool, discardLog())
	if _, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "proj"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, reason := waitChildRun(t, pool, "proj")
	if reason == runref.ReasonCheckoutNeedsAgent || status == "failure" {
		t.Errorf("a project step with an agent to wait for was refused: status %q, reason %q", status, reason)
	}
}
