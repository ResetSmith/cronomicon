package workflow

import (
	"context"
	"testing"
)

// 2.5.0 C11, defect 79: a workflow built in the app is at home in its owner
// agency. Its name-only steps (saved before 2.4.0 pinned them, or through the
// API) are that agency's jobs, an app job or a Git one of its repository, so
// another agency's job of the same name neither runs nor makes the step
// ambiguous. Until C11 it had no home, the name was counted among every
// agency's, and C's job called backup refused B's step.
func TestC11_AnInAppWorkflowsNameOnlyStepIsItsAgencys(t *testing.T) {
	f := newHomeFixture(t)
	f.exec(`INSERT INTO jobs (uid, name, source, run_type, command, concurrency_policy, enabled, scope, synced_at, owner_agency, repo_id) VALUES
	        ('j-b',  'backup',  'cronomicon', 'bash', 'echo b', 'Allow', 1, 'b-hosts', 't', 'ag-b', NULL),
	        ('j-c',  'backup',  'cronomicon', 'bash', 'echo c', 'Allow', 1, 'c-hosts', 't', 'ag-c', NULL),
	        ('j-gb', 'archive', 'git',        'bash', 'echo b', 'Allow', 1, 'b-hosts', 't', 'ag-b', 'repo-b'),
	        ('j-gc', 'archive', 'git',        'bash', 'echo c', 'Allow', 1, 'c-hosts', 't', 'ag-c', 'repo-c')`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, owner_agency) VALUES ('w-b2', 'b-nightly', 'cronomicon', '[]', 1, 't', 'ag-b')`)
	var wf int64
	if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = 'w-b2'`).Scan(&wf); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ step, source, want string }{
		{"backup", "", "j-b"},      // the app's job of B, not C's of the name
		{"archive", "git", "j-gb"}, // B's repository's, not C's
	} {
		if _, err := f.e.Trigger(context.Background(), TriggerParams{
			WorkflowName: "b-nightly", WorkflowSource: "cronomicon", WorkflowID: wf, TriggeredBy: "scheduler",
			Steps: []Step{{Type: "job", Name: c.step, Source: c.source}},
		}); err != nil {
			t.Fatalf("Trigger: %v", err)
		}
		uid, _, status, reason, _, ok := f.lastRun(c.step)
		if !ok || uid != c.want {
			t.Errorf("B's workflow naming %s: ran job %q (%s %q); want %s", c.step, uid, status, reason, c.want)
		}
	}
}
