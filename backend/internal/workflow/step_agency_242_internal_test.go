package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// Present defects 57, 58 and 61 (found by the review of 2.4.x).
//
// 57: a step whose job resolved to ANOTHER agency's was confined only by its
// scope's NAME. With names per agency (I2) B and C may both hold `web`, so B's
// workflow ran C's job, under B's schedule and environment, with C's runners.
//
// 58: a refused step's row was written with the job's agency set, its uid and
// its environment: C was shown a failure of its job it never triggered, and B
// might not see its own step's failure.
//
// 61: a step whose job resolved to nothing became a bash job with no command.
func TestRV_AStepRunsItsWorkflowsAgencysJobOnlyAndARefusedStepIsTheWorkflows(t *testing.T) {
	f := newHomeFixture(t)
	for _, sc := range [][3]string{{"sc-wb", "web", "ag-b"}, {"sc-wc", "web", "ag-c"}} {
		f.exec(`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES (?, ?, 'cronomicon', 't', ?)`, sc[0], sc[1], sc[2])
		f.exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, sc[0])
		f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, sc[0], sc[2])
	}
	f.exec(`INSERT INTO jobs (uid, name, source, run_type, command, concurrency_policy, enabled, scope, synced_at, owner_agency)
	        VALUES ('j-c', 'backup', 'cronomicon', 'bash', 'echo c', 'Allow', 1, 'web', 't', 'ag-c')`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, owner_agency) VALUES ('w-b', 'b-flow', 'cronomicon', '[]', 1, 't', 'ag-b')`)
	var wfB int64
	if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = 'w-b'`).Scan(&wfB); err != nil {
		t.Fatal(err)
	}
	run := func(step string, pin ...string) (uid, status, reason, agencies, env string) {
		t.Helper()
		s := Step{Type: "job", Name: step}
		if len(pin) > 0 {
			s.JobUID = pin[0]
		}
		if _, err := f.e.Trigger(context.Background(), TriggerParams{
			WorkflowName: "b-flow", WorkflowSource: "cronomicon", WorkflowID: wfB, TriggeredBy: "scheduler",
			Steps: []Step{s},
		}); err != nil {
			t.Fatalf("Trigger: %v", err)
		}
		uid, _, status, reason, env, ok := f.lastRun(step)
		if !ok {
			t.Fatalf("no run row for the step %s", step)
		}
		if err := f.e.db.QueryRow(`SELECT COALESCE(agencies_json, '') FROM runs WHERE job_name = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, step).Scan(&agencies); err != nil {
			t.Fatal(err)
		}
		return uid, status, reason, agencies, env
	}

	// Pinned to C's job by its uid: found, and refused as C's.
	uid, status, reason, agencies, env := run("backup", "j-c")
	if status != "failure" || reason != runref.ReasonStepJobOtherAgency {
		t.Errorf("B's workflow running C's backup (both agencies hold a scope web): %q, %q; want it refused with %q", status, reason, runref.ReasonStepJobOtherAgency)
	}
	if uid != "" || agencies != `["B"]` {
		t.Errorf("the refused step's row is job %q, agencies %s; want no job, B's", uid, agencies)
	}
	if env != "" && env != "{}" {
		t.Errorf("the refused step's row carries an environment: %s", env)
	}

	// By its name only: B's workflow built in the app is at home in B (2.5.0
	// C11, defect 79), so C's job of the name is not found at all. (The walk is
	// asynchronous: the pinned run's row goes first, so lastRun waits for this one.)
	f.exec(`DELETE FROM runs WHERE job_name = 'backup'`)
	_, status, reason, _, _ = run("backup")
	if status != "failure" || reason != runref.ReasonStepJobNotFound {
		t.Errorf("B's workflow naming backup, which only C holds: %q, %q; want %q", status, reason, runref.ReasonStepJobNotFound)
	}

	_, status, reason, agencies, _ = run("no-such-job")
	if status != "failure" || reason != runref.ReasonStepJobNotFound || agencies != `["B"]` {
		t.Errorf("a step naming no job: %q, %q, %s; want it refused with %q under B", status, reason, agencies, runref.ReasonStepJobNotFound)
	}

	// A sub-workflow of C's: refused, and its row names no workflow of C's.
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, owner_agency) VALUES ('w-c', 'c-flow', 'cronomicon', '[]', 1, 't', 'ag-c')`)
	if _, err := f.e.Trigger(context.Background(), TriggerParams{
		WorkflowName: "b-flow", WorkflowSource: "cronomicon", WorkflowID: wfB, TriggeredBy: "scheduler",
		Steps: []Step{{Type: "workflow", Name: "go", Workflow: "c-flow", WorkflowUID: "w-c"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	var childUID, childReason string
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.e.db.QueryRow(`SELECT COALESCE(workflow_uid, ''), COALESCE(queued_reason, '') FROM workflow_runs
		                            WHERE workflow_name = 'c-flow' AND parent_workflow_run_id IS NOT NULL`).Scan(&childUID, &childReason); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childReason != ReasonChildOtherAgency || childUID != "" {
		t.Errorf("B's workflow's child of C's: reason %q, workflow %q; want refused with %q and no workflow named", childReason, childUID, ReasonChildOtherAgency)
	}
}
