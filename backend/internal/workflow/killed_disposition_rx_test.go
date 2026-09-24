package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// RX-8 — the payoff of Phase A, and the one behaviour change a user will notice.
//
// The engine's step-outcome switch folds `killed` into danger alongside
// `failure`. That fold is correct for an UNCLASSIFIED stop and stays (RX-Q7).
// What changes is that a stop can now write a different status entirely, so the
// engine reads what the operator recorded rather than inferring failure from the
// fact that a human was involved.
//
// These tests deliberately assert on the engine as it stands: no code in
// engine.go moved for RX-8, because the switch already reads `status` and a
// disposition changes `status` itself. That makes them regression fences rather
// than proofs of new code — if someone later "simplifies" the engine to branch
// on killed_by, or re-folds success-dispositioned stops into danger, the feature
// silently dies and only these tests notice.

// stopRun simulates an operator stopping a queued child run with a disposition:
// the chosen status plus killed_by, exactly what the kill route writes.
func stopRun(pool *sql.DB, wfTraceID, jobName, disposition string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_, _ = pool.Exec(`
					UPDATE runs SET status = ?, killed_by = 'ops@example.com', completed_at = ?
					WHERE workflow_run_id = ? AND job_name = ? AND status IN ('queued','running')`,
					disposition, "2026-01-01T00:00:00Z", wfTraceID, jobName)
			}
		}
	}()
	return func() { close(done) }
}

// simulateJobRuns is simulateRuns narrowed to ONE job name, so it cannot race
// the operator stand-in above for a run that a test wants stopped rather than
// completed. Without the narrowing the generic simulator can win the row first
// and overwrite the disposition, which looks exactly like a product bug.
func simulateJobRuns(pool *sql.DB, wfTraceID, jobName, status string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_, _ = pool.Exec(`
					UPDATE runs SET status = ?, completed_at = ?
					WHERE workflow_run_id = ? AND job_name = ? AND status = 'queued'`,
					status, "2026-01-01T00:00:00Z", wfTraceID, jobName)
			}
		}
	}()
	return func() { close(done) }
}

// TestKilledStepDispositionedSuccessLetsTheWorkflowProceed — impossible before
// Phase A.
//
// An operator finds step 1 wedged (the work is actually done; the process is
// hung on a socket). Today their only lever fails the whole workflow, so the
// remaining steps never run and the operator has to re-trigger from scratch —
// re-running the wedged step's work. With a disposition they stop it, record
// what was true, and the workflow carries on.
func TestKilledStepDispositionedSuccessLetsTheWorkflowProceed(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "wedged")
	seedJob(t, pool, "downstream")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "stop-success-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{
			{Type: "job", Name: "wedged"},
			{Type: "job", Name: "downstream"},
		},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// The operator stops step 1 and records it as a success; step 2 runs normally.
	// The two simulators must not both claim `wedged`, or the generic one can win
	// the row and clear the disposition's provenance — so the runner stand-in is
	// scoped to the downstream step only.
	stopWedged := stopRun(pool, res.TraceID, "wedged", "success")
	defer stopWedged()
	stopRest := simulateJobRuns(pool, res.TraceID, "downstream", "success")
	defer stopRest()

	status, _ := waitWorkflowTerminal(t, pool, res.TraceID)
	if status != "success" {
		t.Fatalf("workflow status = %q, want success — a step stopped and recorded as a success "+
			"must not fail the workflow (RX-8)", status)
	}

	// The downstream step actually ran. Without this, "success" could just mean
	// the walk halted early and finalised optimistically.
	var downstream int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=? AND job_name='downstream'`,
		res.TraceID).Scan(&downstream)
	if downstream == 0 {
		t.Error("downstream step never ran — the workflow did not actually proceed past the stopped step")
	}

	// The provenance survives: status says success, killed_by says a human did it.
	// Both halves must be true at once, or History cannot tell this apart from an
	// ordinary success (which is exactly what RX-23's marker renders).
	var killedBy sql.NullString
	var stepStatus string
	if err := pool.QueryRow(`SELECT status, killed_by FROM runs
		WHERE workflow_run_id=? AND job_name='wedged'`, res.TraceID).Scan(&stepStatus, &killedBy); err != nil {
		t.Fatal(err)
	}
	if stepStatus != "success" || !killedBy.Valid {
		t.Errorf("stopped step = (status %q, killed_by valid %v), want (success, true) — status is the "+
			"outcome, killed_by is the proof a human ended it", stepStatus, killedBy.Valid)
	}
}

// TestUnclassifiedKilledStepStillFailsTheWorkflow — the other half, and the
// reason RX-8 is not simply "stop treating killed as a failure".
//
// An unclassified stop means the operator said nothing about the work. Inferring
// success from silence would let a stopped deploy's downstream steps run against
// a half-deployed system. `killed` keeps meaning what it always meant.
func TestUnclassifiedKilledStepStillFailsTheWorkflow(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "wedged")
	seedJob(t, pool, "downstream")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "stop-plain-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{
			{Type: "job", Name: "wedged"},
			{Type: "job", Name: "downstream"},
		},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stopWedged := stopRun(pool, res.TraceID, "wedged", "killed")
	defer stopWedged()

	status, _ := waitWorkflowTerminal(t, pool, res.TraceID)
	if status != "failure" {
		t.Fatalf("workflow status = %q, want failure — an UNCLASSIFIED stop must still halt the "+
			"workflow; silence is not consent (RX-Q7)", status)
	}

	var downstream int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE workflow_run_id=? AND job_name='downstream' AND status != 'skipped'`,
		res.TraceID).Scan(&downstream)
	if downstream != 0 {
		t.Errorf("downstream ran %d non-skipped times after an unclassified stop, want 0", downstream)
	}
}

// TestKilledStepDispositionedWarningProceeds — `warning` is a completed state in
// the engine's switch ("success", "warning" → success), so a stop recorded as a
// warning proceeds like a warning-exiting run. Pinned because the engine's
// success arm and statusLabel's Warn arm are separately maintained, and a change
// to either could silently reclassify this.
func TestKilledStepDispositionedWarningProceeds(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "wedged")
	seedJob(t, pool, "downstream")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "stop-warn-wf", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{
			{Type: "job", Name: "wedged"},
			{Type: "job", Name: "downstream"},
		},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stopWedged := stopRun(pool, res.TraceID, "wedged", "warning")
	defer stopWedged()
	stopRest := simulateJobRuns(pool, res.TraceID, "downstream", "success")
	defer stopRest()

	status, _ := waitWorkflowTerminal(t, pool, res.TraceID)
	if status != "success" {
		t.Errorf("workflow status = %q, want success — a stop recorded as a warning is a completed "+
			"step, matching a warning-exiting run", status)
	}
}
