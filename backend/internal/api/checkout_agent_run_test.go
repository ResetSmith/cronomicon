package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// Present defect 44, the manual and token half: a run of a project job is
// refused, 422 checkout_requires_agent with what to do about it and no run
// row, when no registered agent that serves the job allows checkout. With one
// registered it is accepted and waits for it.
func TestRunOfAProjectJobWithNoCheckoutAgentIsRefused(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's
	exec := mustExec(t, pool)
	exec(`UPDATE git_repos SET last_sha = '1111111111111111111111111111111111111111' WHERE id = 'global'`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, script_path, project_root, scope, enabled, synced_at, repo_id)
	      VALUES ('j-proj', 'proj-job', 'git', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'fin-hosts', 1, 't', 'global')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id)
	      VALUES ('j-body', 'body-job', 'git', 'bash', 'echo hi', 'fin-hosts', 1, 't', 'global')`)
	path := runPath(t, pool, "proj-job")
	runs := func(job string) int {
		t.Helper()
		return count(t, pool, `SELECT COUNT(*) FROM runs WHERE job_name = ?`, job)
	}

	rec := gateReq(t, h, http.MethodPost, path, gRoot, "")
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusUnprocessableEntity || e.Code != runref.CodeCheckoutNeedsAgent || !strings.Contains(e.Message, "-allow-checkout") {
		t.Fatalf("a run of a project job that no agent can take = %d %q (%s), want 422 %s and what to do", rec.Code, e.Code, rec.Body, runref.CodeCheckoutNeedsAgent)
	}
	if runs("proj-job") != 0 {
		t.Errorf("the refused run left a run row")
	}
	// A job that is no project, on the same scope, is not this refusal's.
	if rec := gateReq(t, h, http.MethodPost, runPath(t, pool, "body-job"), gRoot, ""); rec.Code/100 != 2 {
		t.Errorf("a run of a job that is no project = %d, want it accepted (%s)", rec.Code, rec.Body)
	}

	// An agent of that scope's agency that allows checkout is registered.
	exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
	      VALUES ('fin-checkout', 'fin-checkout', 'agent', 'offline', 'Linux', '["ansible","checkout"]', 0, 5, '1.0', 't', 't')`)
	exec(`INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES ('fin-checkout', 'ag:FIN')`)
	if rec := gateReq(t, h, http.MethodPost, path, gRoot, ""); rec.Code/100 != 2 {
		t.Fatalf("a run of the project job with an agent to wait for = %d, want it accepted (%s)", rec.Code, rec.Body)
	}
	if runs("proj-job") != 1 {
		t.Errorf("the accepted run left %d rows, want 1", runs("proj-job"))
	}
}
