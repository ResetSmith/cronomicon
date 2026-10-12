package runner

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// TestGR0_RemovingItsScriptLiftsAnInAppJobsConfinement pins present defect 45
// (found by the second review of Phase R5 on 2026-10-12, reproduced here, not
// changed: it needs the repository a job's copy came from to be recorded on
// the job, which is a migration and the owner's to decide).
//
// A job built in the app on Finance's script whose body is an INLINE copy. Its
// scope is given to Tax, and its runs are refused (present defect 40's rule).
// Finance's committers then delete the script. The job is joined to no script
// any more, so the rule no longer finds a repository to confine it to; and
// its body is its own copy, so it is not "its script has gone" either
// (present defect 41's rule). It runs again, on Tax's hosts, the body Finance
// wrote while the scope was theirs: who is not the job's agency holds the
// switch.
func TestGR0_RemovingItsScriptLiftsAnInAppJobsConfinement(t *testing.T) {
	svc, _, fetch := inAppOnFinance(t)
	exec := func(q string) {
		t.Helper()
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`UPDATE scripts SET script_path = NULL, script = 'echo written by finance' WHERE uid = 's-fin'`)
	exec(`UPDATE jobs SET script_path = NULL, script = 'echo written by finance' WHERE uid = 'j-app'`)
	if rec := fetch(); rec.Code != http.StatusOK {
		t.Fatalf("on Finance's scope the job was answered %d: %s", rec.Code, rec.Body.String())
	}
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	if rec := fetch(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), runref.CodeRepoScopeMismatch) {
		t.Fatalf("with its scope given to Tax the job was answered %d: %s; want it refused", rec.Code, rec.Body.String())
	}

	exec(`DELETE FROM scripts WHERE uid = 's-fin'`)
	rec := fetch()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo written by finance") {
		t.Errorf("PRESENT DEFECT CHANGED: after Finance removed the script, the job on Tax's scope was answered %d: %s. "+
			"It was handed out again with the body Finance wrote; if it now stays refused, invert this test.", rec.Code, rec.Body.String())
	}
}
