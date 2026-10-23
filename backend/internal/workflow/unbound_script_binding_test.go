package workflow_test

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// A workflow step reads the bindings of its job's SCRIPT by the uid on the job
// (jobs.script_uid, migration 1290). A script that binds a department's secret
// fails the unbound step; another script of the same name does not.
func TestWorkflowStepFollowsTheJobsOwnScript(t *testing.T) {
	pool := openPool(t)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	exec(`INSERT INTO secrets (id, key, source, owner_agency, created_at) VALUES ('s-dept', 'DEPT_PASSWORD', 'stored', 'ag-tax', 't')`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-dept', 'ag-tax')`)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib','global','lib.sh','bash','x','h','t')`)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib-other','repo-b','lib.sh','bash','x','h','t')`)
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "script", Name: "lib.sh", UID: "uid-lib"},
		[]runref.Binding{{Kind: runref.KindSecret, Name: "DEPT_PASSWORD"}}, "t"); err != nil {
		t.Fatalf("bind secret to the script: %v", err)
	}

	// The step's job uses the script that binds the department's secret.
	seedJob(t, pool, "uses-own")
	exec(`UPDATE jobs SET script_ref='lib.sh', script_uid='uid-lib' WHERE name='uses-own'`)
	if status := triggerOneStep(t, pool, "uses-own"); status == "success" {
		t.Error("the workflow succeeded on a step whose script's secret could not have resolved")
	}
	if status, reason := stepRun(t, pool, "uses-own"); status != "failure" || reason != runref.QueuedReasonUnboundReferences {
		t.Errorf("the step: status %q, reason %q; want failure, %q", status, reason, runref.QueuedReasonUnboundReferences)
	}

	// The same name, another script: not this job's bindings, so the step is
	// enqueued like any other.
	seedJob(t, pool, "uses-other")
	exec(`UPDATE jobs SET script_ref='lib.sh', script_uid='uid-lib-other' WHERE name='uses-other'`)
	eng := workflow.New(pool, discardLog())
	if _, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent2", WorkflowID: 2, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "uses-other"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, reason := waitChildRun(t, pool, "uses-other")
	if reason == runref.QueuedReasonUnboundReferences || status == "failure" {
		t.Errorf("a step was refused (status %q, reason %q) for a binding of ANOTHER script that shares its script's name", status, reason)
	}
}
