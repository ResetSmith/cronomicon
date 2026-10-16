package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// A workflow built in the app is its owner's, and runs its owner's jobs and
// workflows only (§0a, IN-8). One that spans agencies (saved before §0a, or
// by a global administrator under GR-7) fails its other agencies' steps at
// run time, each with the reason, and runs the rest (§0a, open point 1).
//
// Until §0a a workflow built in the app ran any agency's job its steps named.
func TestI1_AWorkflowBuiltInTheAppRunsItsOwnAgencysJobsOnly(t *testing.T) {
	f := newHomeFixture(t)
	f.job("app-b", "b-job", "", "b-hosts")
	f.job("app-c", "c-job", "", "c-hosts")
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency) VALUES
	        ('wf-b', 'spans', 'cronomicon', '[]', 1, 't', 'ag-b')`)
	var row int64
	if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = 'wf-b'`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	run := func(step, uid string) (status, reason string) {
		t.Helper()
		if _, err := f.e.Trigger(context.Background(), TriggerParams{
			WorkflowName: "spans", WorkflowSource: "cronomicon", WorkflowID: row, TriggeredBy: "scheduler",
			Steps: []Step{{Type: "job", Name: step, JobUID: uid}},
		}); err != nil {
			t.Fatalf("Trigger: %v", err)
		}
		_, _, status, reason, _, ok := f.lastRun(step)
		if !ok {
			t.Fatalf("no run row for the step %s", step)
		}
		return status, reason
	}
	// Refused as another agency's job (2.4.2, present defect 57: asked of the
	// job itself, before its scope is).
	if status, reason := run("c-job", "app-c"); status != "failure" || reason != runref.ReasonStepJobOtherAgency {
		t.Errorf("B's workflow running C's job: %q, %q; want it failed with %q", status, reason, runref.ReasonStepJobOtherAgency)
	}
	if status, reason := run("b-job", "app-b"); status == "failure" || strings.HasPrefix(reason, "Refused") {
		t.Errorf("B's workflow running B's job: %q, %q; want it queued", status, reason)
	}
}

// A sub-workflow is its parent's agency's (§0a, IN-8): sameAgency, which the
// sub-workflow step asks before it starts the child.
func TestI1_ASubWorkflowIsItsParentsAgencys(t *testing.T) {
	f := newHomeFixture(t)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency) VALUES
	        ('app-b', 'b-flow', 'cronomicon', '[]', 1, 't', 'ag-b'),
	        ('app-c', 'c-flow', 'cronomicon', '[]', 1, 't', 'ag-c'),
	        ('app-g', 'g-flow', 'cronomicon', '[]', 1, 't', 'global')`)
	gitB := f.workflowRow("git-b", "b-git", "repo-b", `[]`)
	gitG := f.workflowRow("git-g", "g-git", "global", `[]`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id) VALUES ('git-gone', 'gone', 'git', '[]', 1, 't', 'repo-gone')`)
	row := func(uid string) int64 {
		var r int64
		if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = ?`, uid).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		what   string
		a, b   int64
		wantOK bool
	}{
		{"B's built in the app and B's repository's", row("app-b"), gitB, true},
		{"Global's built in the app and Global's repository's", row("app-g"), gitG, true},
		{"B's and C's, both built in the app", row("app-b"), row("app-c"), false},
		{"B's and Global's repository's", row("app-b"), gitG, false},
		{"a workflow whose repository is gone", row("git-gone"), row("git-gone"), false},
	} {
		got, err := f.e.sameAgency(context.Background(), c.a, c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.wantOK {
			t.Errorf("%s: same agency = %v, want %v", c.what, got, c.wantOK)
		}
	}
}
