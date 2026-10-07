package workflow_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// RA-24, the workflow-step half: a step whose job has no scope runs as Global's
// work, and Global's run resolves Global's secrets and no department's. A step
// that declares a department's secret therefore cannot run, and it must FAIL —
// visibly, on its child row — rather than be enqueued to die at dispatch.
//
// The engine guarded this with "no scope AND no agencies". Since migration 1220
// an unbound run carries ["Global"], so the second half was never true and the
// refusal silently stopped happening; nothing failed, because nothing tested it.

func seedUnboundStepFixture(t *testing.T, pool *sql.DB, job, secretKey, owner string) {
	t.Helper()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seedJob(t, pool, job)
	exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	// owner "" leaves the secret where every row is born: Global's, owner and member.
	exec(`INSERT INTO secrets (id, key, source, owner_agency, created_at) VALUES (?, ?, 'stored', ?, 't')`, "s-"+secretKey, secretKey, owner)
	if owner != "" {
		exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, ?)`, "s-"+secretKey, owner)
	}
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "job", Source: "git", Name: job},
		[]runref.Binding{{Kind: runref.KindSecret, Name: secretKey}}, "t"); err != nil {
		t.Fatalf("bind secret: %v", err)
	}
}

func TestWorkflowStep_RefusesAnUnboundStepThatNeedsADepartmentsSecret(t *testing.T) {
	pool := openPool(t)
	seedUnboundStepFixture(t, pool, "unscoped", "DEPT_PASSWORD", "ag-tax")

	if status := triggerOneStep(t, pool, "unscoped"); status == "success" {
		t.Error("the workflow succeeded on a step that could not have resolved its secret")
	}
	status, reason := stepRun(t, pool, "unscoped")
	if status != "failure" {
		t.Errorf("child run status = %q, want failure", status)
	}
	if reason != runref.QueuedReasonUnboundReferences {
		t.Errorf("queued_reason = %q, want %q", reason, runref.QueuedReasonUnboundReferences)
	}
}

// The control: the same step bound to a secret that is Global's is Global's to
// run, and is enqueued as before.
func TestWorkflowStep_AnUnboundStepWithAGlobalSecretIsEnqueued(t *testing.T) {
	pool := openPool(t)
	seedUnboundStepFixture(t, pool, "unscoped", "SHARED_TOKEN", "")

	eng := workflow.New(pool, discardLog())
	if _, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "unscoped"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, reason := waitChildRun(t, pool, "unscoped")
	if reason == runref.QueuedReasonUnboundReferences || status == "failure" {
		t.Errorf("a step bound to Global's own secret was refused: status %q, reason %q", status, reason)
	}
	var agencies string
	if err := pool.QueryRow(`SELECT agencies_json FROM runs WHERE job_name = 'unscoped'`).Scan(&agencies); err != nil || agencies != `["Global"]` {
		t.Errorf("the unbound step's run carries %q (%v), want [\"Global\"]", agencies, err)
	}
}
