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

// The same home where a workflow is READ, where a run of it is cancelled, and
// where a parked run of it is judged: each asked without it until the review
// of Phase R4 showed that only the trigger was tested. With a job of one name
// in two agencies' repositories and no home, the step is ambiguous and has no
// scope: the workflow is then an unrestricted operator's alone.
func TestAGitWorkflowsHomeIsAskedWhereverItIsAuthorized(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'u', 'main'), ('repo-tax', 'ag:TAX', 'u', 'main')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id) VALUES
	      ('d-fin', 'deploy', 'git', 'bash', 'echo fin', 'fin-hosts', 1, 't', 'repo-fin'),
	      ('d-tax', 'deploy', 'git', 'bash', 'echo tax', 'tax-hosts', 1, 't', 'repo-tax')`)
	// TAX's repository has a workflow of the same name too, and it is the older
	// row: asked with no home, a sub-workflow step naming `nightly` means that one.
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id, owner_agency) VALUES
	      ('wf-tax', 'nightly', 'git', '[{"type":"job","name":"deploy"}]', 1, 't', 'repo-tax', 'ag:TAX')`)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id, owner_agency) VALUES
	      ('wf-fin', 'nightly', 'git', '[{"type":"job","name":"deploy"}]', 1, 't', 'repo-fin', 'ag:FIN'),
	      ('wf-fin-parent', 'weekly', 'git', '[{"type":"workflow","name":"go","workflow":"nightly"}]', 1, 't', 'repo-fin', 'ag:FIN')`)
	id := rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = 'wf-fin'`)
	parent := rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = 'wf-fin-parent'`)

	// Whether it is visible (what its tags and its annotation ask): to FIN's
	// administrator, and not to TAX's.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflow-tags/"+id, gTaxAdmin, `{"tags":["x"]}`); rec.Code/100 == 2 {
		t.Errorf("TAX's administrator tagging FIN's repository's workflow = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflow-tags/"+id, gFinAdmin, `{"tags":["x"]}`); rec.Code/100 != 2 {
		t.Errorf("FIN's administrator tagging FIN's repository's workflow = %d, want it accepted (%s)", rec.Code, rec.Body)
	}

	// Cancelling a run of the parent, whose sub-workflow's job is yet to be
	// reached: FIN's operator may. (The run is written as the engine leaves one
	// that is waiting on its first step.)
	exec(`INSERT INTO workflow_runs (id, workflow_id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind,
	                                 steps_snapshot, started_at, created_at)
	      VALUES ('wr-1', ?, 'weekly', 'git', 'wf-fin-parent', 'running', 'scheduler', 'scheduled',
	              '[{"type":"workflow","name":"go","workflow":"nightly"}]', 't', 't')`, parent)
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows/runs/wr-1/cancel", gTaxAdmin, `{}`); rec.Code/100 == 2 {
		t.Errorf("TAX's administrator cancelling a run of FIN's repository's workflow = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows/runs/wr-1/cancel", gFinOperator, `{}`); rec.Code/100 != 2 {
		t.Errorf("FIN's operator cancelling a run of FIN's repository's workflow = %d, want it accepted (%s)", rec.Code, rec.Body)
	}
}

// And a PARKED run of a Git workflow: who may cancel it is judged on the jobs
// the workflow would run from its own repository.
func TestAParkedRunOfAGitWorkflowIsJudgedFromItsHome(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'u', 'main'), ('repo-tax', 'ag:TAX', 'u', 'main')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id) VALUES
	      ('d-fin', 'deploy', 'git', 'bash', 'echo fin', 'fin-hosts', 1, 't', 'repo-fin'),
	      ('d-tax', 'deploy', 'git', 'bash', 'echo tax', 'tax-hosts', 1, 't', 'repo-tax')`)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id, owner_agency)
	      VALUES ('wf-fin', 'nightly', 'git', '[{"type":"job","name":"deploy"}]', 1, 't', 'repo-fin', 'ag:FIN')`)
	exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, owner_uid)
	      VALUES ('p-wf', 'workflow', 'nightly', 'git', '', '2099-01-01T00:00:00Z', 'ops@example', '2026-01-01T00:00:00Z', 'wf-fin')`)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/pending-runs/p-wf", gTaxAdmin, ""); rec.Code == http.StatusNoContent {
		t.Fatalf("TAX's administrator cancelled a parked run of FIN's repository's workflow")
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/pending-runs/p-wf", gFinOperator, ""); rec.Code != http.StatusNoContent {
		t.Errorf("FIN's operator cancelling a parked run of FIN's repository's workflow = %d, want 204 (%s)", rec.Code, rec.Body)
	}
}
