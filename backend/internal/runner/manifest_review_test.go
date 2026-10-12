package runner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// After the independent review of 2.4.0 Phase R5.

// The dispatch-time confinement asked about the JOB's repository and not the
// WORKFLOW's. A step of an agency's repository's workflow may run a job of
// Global's repository (GR-16), which the job's check never refuses; the engine
// asks about the workflow's repository when the step is queued. A scope given
// to another agency after that left the step to run on the other agency's
// hosts, on the first agency's schedule and with its environment.
func TestManifestRefusesAWorkflowsStepWhoseScopeWasGivenAway(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-wfconf", "crn_run_wfconf"
	insertRunner(t, svc, runnerID, "wfconf", "online", []string{"terraform"})
	bindRunnerToken(t, svc, tok, runnerID)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	// Global's job, which may name any agency's scope.
	exec(`INSERT INTO jobs (uid, name, source, repo_id, scope, run_type, command, synced_at) VALUES
	      ('j-global', 'global-plan', 'git', 'global', 'fin-hosts', 'terraform', 'plan -var=global', 't')`)
	// Three workflows that run it: Finance's repository's, Global's, and one
	// built in the app. Each has a run under way.
	exec(`INSERT INTO workflows (uid, name, source, repo_id, steps, synced_at) VALUES
	      ('wf-fin',    'fin-flow',    'git',        'repo-fin', '[]', 't'),
	      ('wf-global', 'global-flow', 'git',        'global',   '[]', 't'),
	      ('wf-app',    'app-flow',    'cronomicon', NULL,       '[]', 't')`)
	for _, wf := range [][3]string{{"wfrun-fin", "fin-flow", "wf-fin"}, {"wfrun-global", "global-flow", "wf-global"}, {"wfrun-app", "app-flow", "wf-app"}} {
		source := "git"
		if wf[2] == "wf-app" {
			source = "cronomicon"
		}
		exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at)
		      VALUES (?, ?, ?, ?, 'running', 'test', 'manual', ?)`, wf[0], wf[1], source, wf[2], now())
	}
	step := func(workflowRun any) string {
		t.Helper()
		id := db.NewTraceID()
		exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
		                       workflow_run_id, triggered_by, trigger_kind, started_at, created_at)
		      VALUES (?, 'global-plan', 'git', 'j-global', 'terraform', 'fin-hosts', 'running', ?, 'runner', ?, 'test', 'workflow', ?, ?)`,
			id, runnerID, workflowRun, now(), now())
		return id
	}
	fetch := func(traceID string) *httptest.ResponseRecorder {
		h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
		req.SetPathValue("traceId", traceID)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// On Finance's scope, Finance's workflow's step is handed out.
	if rec := fetch(step("wfrun-fin")); rec.Code != http.StatusOK {
		t.Fatalf("the step on its workflow's agency's scope was answered %d: %s", rec.Code, rec.Body.String())
	}

	// The scope is given to Tax with a step of each workflow queued.
	fin, global, app, alone := step("wfrun-fin"), step("wfrun-global"), step("wfrun-app"), step(nil)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)

	rec := fetch(fin)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), runref.CodeRepoScopeMismatch) ||
		!strings.Contains(rec.Body.String(), runref.ReasonWorkflowScopeMismatch) {
		t.Errorf("the step of Finance's repository's workflow on what is now Tax's scope was answered %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "-var=global") {
		t.Errorf("the refusal carries the job's command: %s", rec.Body.String())
	}
	// Global's workflow, a workflow built in the app, and the job run on its
	// own are not a repository's to confine.
	for what, id := range map[string]string{"Global's workflow's step": global, "an in-app workflow's step": app, "the job run on its own": alone} {
		if rec := fetch(id); rec.Code != http.StatusOK {
			t.Errorf("%s on that scope was answered %d: %s", what, rec.Code, rec.Body.String())
		}
	}
}

// A run that records no job, of a name two jobs hold, is refused by
// ResolveCommand ("nothing is guessed"). The manifest answered that with a
// bare 500; it says why, as the local runner does.
func TestManifestSaysWhyAnAmbiguousRunIsRefused(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-amb", "crn_run_amb"
	insertRunner(t, svc, runnerID, "amb", "online", []string{"terraform"})
	bindRunnerToken(t, svc, tok, runnerID)
	seedTwin(t, svc, "uid-plan-a", "plan", "terraform", "plan\n-var=agency=a", 111, `["A_ONLY"]`)
	seedTwin(t, svc, "uid-plan-b", "plan", "terraform", "plan\n-var=agency=b", 222, `["B_ONLY"]`)

	id := claimedRunOf(t, svc, "", "plan", "terraform", runnerID)
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+id+"/manifest", nil)
	req.SetPathValue("traceId", id)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	answer := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(answer, "job_ambiguous") || !strings.Contains(answer, "two jobs are called") {
		t.Fatalf("a run with no uid, of a name two jobs hold = %d %s, want 409 job_ambiguous and the reason", rec.Code, answer)
	}
	if strings.Contains(answer, "agency=a") || strings.Contains(answer, "agency=b") {
		t.Errorf("the refusal carries a job's command: %s", answer)
	}
}
