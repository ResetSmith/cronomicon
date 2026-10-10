package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// What the review of Phase R4 found the engine still let through, and its
// fixes. A workflow of an agency's repository is confined to that agency in
// what it names AND in what it runs on.

// A step of an agency's repository's workflow runs on that agency's scopes,
// whoever's job it resolved to. Its name may mean a job of GLOBAL's repository
// (GR-16), and a Global job may name any agency's scope (GR-14): without this
// an agency's committers ran Global's job on another agency's hosts, on their
// own schedule and with their own environment.
func TestAStepOfAnAgencysWorkflowRunsOnThatAgencysScopesOnly(t *testing.T) {
	f := newHomeFixture(t)
	f.job("g-on-c", "patch-c", "global", "c-hosts")
	f.job("g-on-b", "patch-b", "global", "b-hosts")
	f.job("g-free", "patch-all", "global", "")
	parentB := f.workflowRow("wf-b", "nightly", "repo-b", `[]`)
	parentG := f.workflowRow("wf-g", "nightly", "global", `[]`)
	run := func(row int64, step string) (status, reason string) {
		t.Helper()
		if _, err := f.e.Trigger(context.Background(), TriggerParams{
			WorkflowName: "nightly", WorkflowSource: "git", WorkflowID: row, TriggeredBy: "scheduler",
			EnvJSON: `{"TARGET":"set-by-the-workflow"}`,
			Steps:   []Step{{Type: "job", Name: step}},
		}); err != nil {
			t.Fatalf("Trigger: %v", err)
		}
		_, _, status, reason, _, ok := f.lastRun(step)
		if !ok {
			t.Fatalf("no run row for the step %s", step)
		}
		return status, reason
	}
	for _, step := range []string{"patch-c", "patch-all"} {
		if status, reason := run(parentB, step); status != "failure" || reason != runref.ReasonWorkflowScopeMismatch {
			t.Errorf("an agency's workflow running Global's %s: %q, %q; want it failed with %q", step, status, reason, runref.ReasonWorkflowScopeMismatch)
		}
	}
	// Global's job on the agency's OWN scope is what GR-16 is for.
	if status, reason := run(parentB, "patch-b"); status == "failure" || strings.HasPrefix(reason, "Refused") {
		t.Errorf("an agency's workflow running Global's job on its own scope: %q, %q; want it queued", status, reason)
	}
	// Global's repository's workflow runs on any scope, as before.
	f.exec(`DELETE FROM runs`)
	if status, reason := run(parentG, "patch-c"); status == "failure" || strings.HasPrefix(reason, "Refused") {
		t.Errorf("Global's workflow running Global's job on an agency's scope: %q, %q; want it queued", status, reason)
	}
}

// End to end, the two ways out the review reproduced: a job built in the app
// by another agency, and a workflow built in the app by another agency, are
// not what a name means to a workflow of an agency's repository.
func TestAnAgencysWorkflowDoesNotReachAnotherAgencysInAppDefinitions(t *testing.T) {
	f := newHomeFixture(t)
	f.job("c-app", "payroll", "", "c-hosts")
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency)
	        VALUES ('c-flow', 'month-end', 'cronomicon', '[{"type":"job","name":"payroll"}]', 1, 't', 'ag-c')`)
	parent := f.workflowRow("wf-b", "nightly", "repo-b", `[]`)
	if _, err := f.e.Trigger(context.Background(), TriggerParams{
		WorkflowName: "nightly", WorkflowSource: "git", WorkflowID: parent, TriggeredBy: "scheduler",
		EnvJSON: `{"TARGET":"set-by-agency-b"}`,
		Steps:   []Step{{Type: "job", Name: "payroll"}, {Type: "workflow", Name: "go", Workflow: "month-end"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// Let the walk finish: the parent ends once both steps are done or refused.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		_ = f.e.db.QueryRow(`SELECT status FROM workflow_runs WHERE workflow_name = 'nightly'`).Scan(&status)
		if status != "" && status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var queued int
	if err := f.e.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_uid = 'c-app' AND status IN ('queued', 'running')`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("another agency's in-app job was queued %d time(s) by a workflow of repo-b", queued)
	}
	var started int
	if err := f.e.db.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_name = 'month-end' AND status <> 'failure' AND status <> 'danger'
	                              AND workflow_id = (SELECT rowid FROM workflows WHERE uid = 'c-flow')`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started != 0 {
		t.Errorf("another agency's in-app workflow was started by a workflow of repo-b")
	}
}

// A running parent's sub-workflow is its own repository's, and that child's
// job its own repository's too: the lookup the running engine makes, which is
// not the one authorization makes.
func TestARunningGitParentStartsItsOwnRepositorysChild(t *testing.T) {
	f := newHomeFixture(t)
	f.job("j-c", "work", "repo-c", "c-hosts")
	f.job("j-b", "work", "repo-b", "b-hosts")
	rowC := f.workflowRow("f-c", "flow", "repo-c", `[{"type":"job","name":"work"}]`)
	rowB := f.workflowRow("f-b", "flow", "repo-b", `[{"type":"job","name":"work"}]`)
	parent := f.workflowRow("p-b", "nightly", "repo-b", `[{"type":"workflow","name":"go","workflow":"flow"}]`)
	if _, err := f.e.Trigger(context.Background(), TriggerParams{
		WorkflowName: "nightly", WorkflowSource: "git", WorkflowID: parent, TriggeredBy: "scheduler",
		Steps: []Step{{Type: "workflow", Name: "go", Workflow: "flow"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	uid, scope, _, reason, _, ok := f.lastRun("work")
	var child int64
	_ = f.e.db.QueryRow(`SELECT COALESCE(workflow_id, 0) FROM workflow_runs WHERE workflow_name = 'flow' LIMIT 1`).Scan(&child)
	if !ok || child != rowB || uid != "j-b" || scope != "b-hosts" {
		t.Errorf("the running parent started workflow row %d (its repository's is %d, another's %d) and the job %q on %q (%s); want its own of both",
			child, rowB, rowC, uid, scope, reason)
	}
}

// A step's environment is its own job's (present defect 37, in the released
// code; found by the review of 2.4.0's Phase R4). It was read by name and
// source: with two jobs of one name, a step ran one job with whichever job's
// environment the query returned. Two agencies' in-app jobs may share a name
// since R2-5, and two repositories' Git jobs since 2.4.0.
func TestAStepsEnvironmentIsItsOwnJobs(t *testing.T) {
	f := newHomeFixture(t)
	// The other job of the name is the older row, which is the one the old
	// query returned.
	f.job("d-c", "deploy", "repo-c", "c-hosts")
	f.job("d-b", "deploy", "repo-b", "b-hosts")
	f.exec(`UPDATE jobs SET env_json = '{"C_ONLY":"agency-c-value"}' WHERE uid = 'd-c'`)
	f.exec(`UPDATE jobs SET env_json = '{"B_ONLY":"agency-b-value"}' WHERE uid = 'd-b'`)
	row := f.workflowRow("wf-b", "nightly", "repo-b", `[]`)
	if _, err := f.e.Trigger(context.Background(), TriggerParams{
		WorkflowName: "nightly", WorkflowSource: "git", WorkflowID: row, TriggeredBy: "scheduler",
		Steps: []Step{{Type: "job", Name: "deploy"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	uid, _, _, _, env, ok := f.lastRun("deploy")
	if !ok || uid != "d-b" {
		t.Fatalf("the step's run is of %q (found %v), want d-b", uid, ok)
	}
	if strings.Contains(env, "C_ONLY") || !strings.Contains(env, "B_ONLY") {
		t.Errorf("the step's environment is %s; want its own job's (B_ONLY) and not the other job's of the name (C_ONLY)", env)
	}

	// And for two jobs built in the app, which the released code has: a step
	// pinned to one runs with that one's environment.
	f.job("t-old", "twin", "", "c-hosts")
	f.job("t-new", "twin", "", "b-hosts")
	f.exec(`UPDATE jobs SET env_json = '{"OLD_ONLY":"1"}' WHERE uid = 't-old'`)
	f.exec(`UPDATE jobs SET env_json = '{"NEW_ONLY":"1"}' WHERE uid = 't-new'`)
	got := f.e.childEnvJSON(context.Background(), "no-such-run", "cronomicon", "twin", "t-new", nil)
	if strings.Contains(got.String, "OLD_ONLY") || !strings.Contains(got.String, "NEW_ONLY") {
		t.Errorf("the environment of a step pinned to one twin is %s; want that twin's", got.String)
	}
}
