package gitlab

import (
	"strings"
	"testing"
)

// 2.5.3 CA4b (CA-30): a job's and a workflow's own calendars in Git,
// spec.forbidCalendars and spec.allowCalendars, validated at sync as an
// entry's binding is (a name the agency lacks refuses the definition; a
// freeze calendar is never an Allow), stored on the definition, and checked
// offline for a name in both.
func TestCA4b_AFilesOwnCalendarsAreValidatedAndStored(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "ag-a's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	const job = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: guarded\nspec:\n  run_type: bash\n  command: echo hi\n  scope: hosts-of-repo-b\n" +
		"  forbidCalendars: [change-days]\n"
	gitCommitFile(t, repoB, remoteB, "jobs/guarded.yaml", job, "a job forbidding a calendar its agency lacks")
	res := grSync(t, b, "the agency's repository")
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, "unknown calendar") {
		t.Errorf("the sync: %s (%s); want partial, the calendar unknown to the agency", res.Status, res.ErrorMessage)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE repo_id = 'repo-b' AND name = 'guarded'`); n != 0 {
		t.Errorf("the job forbidding an unknown calendar was synced")
	}

	for _, q := range []string{
		`INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES ('cronomicon', 'change-days', 0, 0, 't', 't', 'ag-b')`,
		`INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES ('cronomicon', 'freeze', 1, 0, 't', 't', 'ag-b')`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	grBackdateAll(t, a)
	if res := grSync(t, b, "with the agency's calendar"); res.Status != "success" {
		t.Fatalf("the sync with the agency's own calendar: %s (%s)", res.Status, res.ErrorMessage)
	}
	if got := grString(t, a.db, `SELECT COALESCE(forbid_calendars, '') FROM jobs WHERE repo_id = 'repo-b' AND name = 'guarded'`); got != `["change-days"]` {
		t.Errorf("the job's forbid_calendars = %q", got)
	}

	// A freeze calendar as a workflow's Allow is refused.
	const wf = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: keeps-freeze\nspec:\n  allowCalendars: [freeze]\n  steps:\n    - type: job\n      name: guarded\n"
	gitCommitFile(t, repoB, remoteB, "workflows/keeps-freeze.yaml", wf, "a workflow keeping to a freeze calendar")
	grBackdateAll(t, a)
	res = grSync(t, b, "with the workflow")
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, "global") {
		t.Errorf("a workflow allowing a freeze calendar: %s (%s); want partial", res.Status, res.ErrorMessage)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM workflows WHERE name = 'keeps-freeze'`); n != 0 {
		t.Errorf("the workflow keeping to a freeze calendar was synced")
	}
	const wfOK = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: keeps-freeze\nspec:\n  allowCalendars: [change-days]\n  steps:\n    - type: job\n      name: guarded\n"
	gitCommitFile(t, repoB, remoteB, "workflows/keeps-freeze.yaml", wfOK, "keep to the change days")
	grBackdateAll(t, a)
	if res := grSync(t, b, "with the workflow fixed"); res.Status != "success" {
		t.Fatalf("the fixed workflow: %s (%s)", res.Status, res.ErrorMessage)
	}
	if got := grString(t, a.db, `SELECT COALESCE(allow_calendars, '') FROM workflows WHERE name = 'keeps-freeze'`); got != `["change-days"]` {
		t.Errorf("the workflow's allow_calendars = %q", got)
	}

	// Offline: a name in both lists.
	errs, _ := validateYAMLBytes("jobs/both.yaml", []byte("apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: both\nspec:\n  run_type: bash\n  command: echo\n"+
		"  forbidCalendars: [x]\n  allowCalendars: [x]\n"))
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "both forbidCalendars and allowCalendars") {
			found = true
		}
	}
	if !found {
		t.Errorf("the validator did not refuse a calendar in both lists: %v", errs)
	}
}
