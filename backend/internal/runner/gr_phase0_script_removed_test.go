package runner

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// TestRemovingItsScriptStopsAnInAppJob (present defect 45; this was the pin
// TestGR0_RemovingItsScriptLiftsAnInAppJobsConfinement, inverted on the
// owner's decision of 2026-10-13).
//
// A job built in the app on Finance's script whose body is an INLINE copy. Its
// scope is given to Tax, and its runs are refused (present defect 40's rule).
// Finance's committers then delete the script. The job was joined to no script
// any more, so that rule found no repository to confine it to, and its body
// was its own copy: it ran again, on Tax's hosts, the body Finance wrote.
//
// A job built in the app runs its script as the server last synced it, and
// with no script it does not run: removing the script can only stop the job.
func TestRemovingItsScriptStopsAnInAppJob(t *testing.T) {
	svc, _, fetch := inAppOnFinance(t)
	exec := func(q string) {
		t.Helper()
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	// The script is an inline one; the job's copy of it is older.
	exec(`UPDATE scripts SET script_path = NULL, script = 'echo written by finance, as synced' WHERE uid = 's-fin'`)
	exec(`UPDATE jobs SET script_path = NULL, script = 'echo the job''s older copy' WHERE uid = 'j-app'`)
	rec := fetch()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo written by finance, as synced") ||
		strings.Contains(rec.Body.String(), "older copy") {
		t.Fatalf("on Finance's scope the job was answered %d: %s; want its script's body as synced, not the job's copy", rec.Code, rec.Body.String())
	}

	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	if rec := fetch(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), runref.CodeRepoScopeMismatch) {
		t.Fatalf("with its scope given to Tax the job was answered %d: %s; want it refused", rec.Code, rec.Body.String())
	}

	// Finance removes the script: the job stays stopped, for another reason.
	exec(`DELETE FROM scripts WHERE uid = 's-fin'`)
	rec = fetch()
	answer := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(answer, "script_gone") {
		t.Fatalf("after Finance removed the script, the job on Tax's scope was answered %d: %s; want 409 script_gone", rec.Code, answer)
	}
	if strings.Contains(answer, "written by finance") || strings.Contains(answer, "older copy") {
		t.Errorf("the refusal carries a body: %s", answer)
	}
	// And on Finance's own scope too: with no script there is nothing to run.
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	if rec := fetch(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "script_gone") {
		t.Errorf("back on Finance's scope, with its script gone, the job was answered %d: %s; want 409 script_gone", rec.Code, rec.Body.String())
	}
}
