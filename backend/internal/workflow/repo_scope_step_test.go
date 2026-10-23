package workflow_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// GR-15 (2.4.0), the workflow-step half: a step that runs a job of an agency's
// repository on a scope that is not that agency's FAILS, on its child row,
// with the reason; it is not enqueued. The scope was the agency's when the job
// was synced and has been given to another since.

func seedRepoStep(t *testing.T, pool *sql.DB, job string) (strand func()) {
	t.Helper()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seedJob(t, pool, job)
	exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	exec(`INSERT OR IGNORE INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	exec(`UPDATE jobs SET repo_id = 'repo-fin', scope = 'fin-hosts' WHERE name = ?`, job)
	return func() {
		t.Helper()
		exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
		exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	}
}

func TestWorkflowStep_RefusesAJobWhoseScopeIsNotItsRepositorysAgencys(t *testing.T) {
	pool := openPool(t)
	strand := seedRepoStep(t, pool, "confined")
	strand()

	if status := triggerOneStep(t, pool, "confined"); status == "success" {
		t.Error("the workflow succeeded on a step whose job may not run on that scope")
	}
	status, reason := stepRun(t, pool, "confined")
	if status != "failure" || reason != runref.ReasonRepoScopeMismatch {
		t.Errorf("the step's run is %q with reason %q; want failure, %q", status, reason, runref.ReasonRepoScopeMismatch)
	}
}

// The control: on its agency's scope the step is enqueued as before.
func TestWorkflowStep_AJobOnItsRepositorysAgencysScopeIsEnqueued(t *testing.T) {
	pool := openPool(t)
	seedRepoStep(t, pool, "confined")

	eng := workflow.New(pool, discardLog())
	if _, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "confined"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, reason := waitChildRun(t, pool, "confined")
	if reason == runref.ReasonRepoScopeMismatch || status == "failure" {
		t.Errorf("a step on its repository's agency's scope was refused: status %q, reason %q", status, reason)
	}
}
