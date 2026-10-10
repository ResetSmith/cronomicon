package api_test

import (
	"net/http"
	"testing"
)

// A Git workflow is authorized on the jobs the engine would run for it: its
// own repository's (2.4.0, GR-16). With a job of one name in two agencies'
// repositories, an operator of the workflow's agency may trigger it, and an
// administrator of the other agency may not. (Asked without the workflow's
// repository the name was ambiguous: the step had no scope to authorize on,
// and nobody but an unrestricted operator could trigger the workflow.)
func TestAGitWorkflowIsAuthorizedOnItsOwnRepositorysJobs(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'u', 'main'), ('repo-tax', 'ag:TAX', 'u', 'main')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id) VALUES
	      ('d-fin', 'deploy', 'git', 'bash', 'echo fin', 'fin-hosts', 1, 't', 'repo-fin'),
	      ('d-tax', 'deploy', 'git', 'bash', 'echo tax', 'tax-hosts', 1, 't', 'repo-tax')`)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id, owner_agency)
	      VALUES ('wf-fin', 'nightly', 'git', '[{"type":"job","name":"deploy"}]', 1, 't', 'repo-fin', 'ag:FIN')`)
	id := rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = 'wf-fin'`)

	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows/"+id+"/trigger", gTaxAdmin, `{}`); rec.Code/100 == 2 {
		t.Errorf("TAX's administrator triggering FIN's repository's workflow = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM workflow_runs`); n != 0 {
		t.Fatalf("a refused trigger started %d workflow run(s)", n)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows/"+id+"/trigger", gFinOperator, `{}`); rec.Code/100 != 2 {
		t.Errorf("FIN's operator triggering FIN's repository's workflow = %d, want it accepted (%s)", rec.Code, rec.Body)
	}
}
