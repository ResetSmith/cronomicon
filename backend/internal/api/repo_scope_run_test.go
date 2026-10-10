package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// GR-15 (2.4.0), the manual and token half: a job of an agency's repository
// runs on that agency's scopes. Asked on the EFFECTIVE scope, so it refuses a
// job whose own scope was given to another agency after it was synced, and a
// run asked for on another agency's scope, for a global administrator too.
// 422 repo_scope_mismatch, and no run row.
func TestRunOfAnAgencysRepositorysJobOnAnotherAgencysScopeIsRefused(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'https://git.example/fin.git', 'main')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id)
	      VALUES ('j-fin', 'fin-job', 'git', 'bash', 'echo hi', 'fin-hosts', 1, 't', 'repo-fin')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id)
	      VALUES ('j-glob', 'global-job', 'git', 'bash', 'echo hi', 'tax-hosts', 1, 't', 'global')`)
	path := runPath(t, pool, "fin-job")
	runs := func(job string) int {
		t.Helper()
		return count(t, pool, `SELECT COUNT(*) FROM runs WHERE job_name = ?`, job)
	}
	refused := func(what, body string) {
		t.Helper()
		before := runs("fin-job")
		rec := gateReq(t, h, http.MethodPost, path, gRoot, body)
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != http.StatusUnprocessableEntity || e.Code != runref.CodeRepoScopeMismatch {
			t.Errorf("%s = %d %q, want 422 %s (%s)", what, rec.Code, e.Code, runref.CodeRepoScopeMismatch, rec.Body)
		}
		if runs("fin-job") != before {
			t.Errorf("%s left a run row", what)
		}
	}

	// On its agency's scope it runs.
	if rec := gateReq(t, h, http.MethodPost, path, gRoot, ""); rec.Code/100 != 2 {
		t.Fatalf("a run of the job on its agency's scope = %d, want it accepted (%s)", rec.Code, rec.Body)
	}
	if runs("fin-job") != 1 {
		t.Fatalf("the accepted run left %d rows", runs("fin-job"))
	}
	// Asked for on another agency's scope.
	refused("a run asked for on another agency's scope", `{"scope":"tax-hosts"}`)

	// Its own scope is given to another agency.
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc:fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc:fin', 'ag:TAX')`)
	refused("a run of the job after its scope was given to another agency", "")

	// Global's repository's job on that agency's scope is nobody's to refuse here.
	if rec := gateReq(t, h, http.MethodPost, runPath(t, pool, "global-job"), gRoot, ""); rec.Code/100 != 2 {
		t.Errorf("a run of Global's repository's job on an agency's scope = %d, want it accepted (%s)", rec.Code, rec.Body)
	}
}
