package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// 2.5.3 CA6: a job with no scope is its own agency's (CA-5), in the checks
// that read a job's agency. Until CA6 three of them read it as Global's: a
// workflow of one agency running one "spanned" agencies (and a save that made
// one was refused, the API's refuseNewSpan), Global was told it had no runner
// for the agency's shell jobs while the agency was not, and a requirement was
// looked for on Global's agents, so it was reported though the agency's agents
// advertised it.

func TestAWorkflowOfOneAgencyRunningItsScopelessJobDoesNotSpan(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't', 'ag-fin')`)
	mustExec(t, pool, `INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	job := func(uid, name string, scope, owner any) {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, owner_agency) VALUES (?, ?, 'cronomicon', 'bash', ?, 't', ?)`,
			uid, name, scope, owner)
	}
	job("j-fin", "fin-job", "fin-hosts", "ag-fin")
	job("j-own", "own-job", nil, "ag-fin") // Finance's, with no scope
	job("j-free", "free-job", nil, nil)    // no owner: born Global's
	wf := func(uid, steps string) {
		mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at, owner_agency) VALUES (?, ?, 'cronomicon', ?, 't', 'ag-fin')`,
			uid, "wf-"+uid, steps)
	}
	wf("own", `[{"type":"job","name":"fin-job"},{"type":"job","name":"own-job"}]`)
	wf("only-own", `[{"type":"job","name":"own-job"}]`)
	wf("with-free", `[{"type":"job","name":"fin-job"},{"type":"job","name":"free-job"}]`)

	spanning, err := notices.SpanningWorkflows(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range spanning {
		got[w.UID] = w.Agencies
	}
	if a, ok := got["own"]; ok {
		t.Errorf("a Finance workflow running Finance's scopeless job spans %q; want it one agency's", a)
	}
	if a, ok := got["only-own"]; ok {
		t.Errorf("a workflow of Finance's scopeless job alone spans %q", a)
	}
	// A job with no owner at all is still the built-in agency's.
	if a := got["with-free"]; !strings.Contains(a, "Finance") || !strings.Contains(a, "Global") {
		t.Errorf("a Finance job beside an ownerless one: spanning %q, want Finance and Global", a)
	}
}

func TestAnAgencysScopelessShellJobsAreToldToThatAgency(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, owner_agency) VALUES ('j-own', 'own-job', 'cronomicon', 'bash', NULL, 't', 'ag-fin')`)
	// Central's leftover: it runs nothing, and is central_holds_work's.
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES ('j-free', 'free-job', 'cronomicon', 'bash', NULL, 't')`)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindNoRunnerForShellJobs)
	}
	got := check()
	n, ok := got["ag-fin"]
	if !ok || n.AgencyID != "ag-fin" || !strings.Contains(n.Detail, "own-job") {
		t.Fatalf("Finance's scopeless shell job and no runner: notices %v; want one under Finance naming own-job", got)
	}
	if strings.Contains(n.Detail, "free-job") {
		t.Errorf("Finance's notice names the built-in agency's job: %s", n.Detail)
	}
	if g, ok := got["global"]; ok {
		t.Errorf("a no-runner notice under the built-in agency, which runs nothing: %s", g.Detail)
	}

	// A Finance agent that runs bash: the notice clears.
	mustExec(t, pool, `INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
	                   VALUES ('rn-fin', 'fin-agent', 'agent', 'offline', 'Linux', '["bash"]', 0, 1, '1', 't', 't', 'ag-fin')`)
	mustExec(t, pool, `INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES ('rn-fin', 'ag-fin')`)
	if got := check(); len(got) != 0 {
		t.Errorf("with a Finance agent that runs bash: notices %v, want none", got)
	}
}

func TestAScopelessJobsRequirementsAreLookedForOnItsAgencysAgents(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, requires_json, synced_at, owner_agency)
	                   VALUES ('j-py', 'py-job', 'cronomicon', 'bash', NULL, '["python"]', 't', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
	                   VALUES ('rn-fin', 'fin-agent', 'agent', 'offline', 'Linux', '["bash"]', 0, 1, '1', 't', 't', 'ag-fin')`)
	mustExec(t, pool, `INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES ('rn-fin', 'ag-fin')`)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindShellJobRequires)
	}
	// Finance's agent does not advertise python: reported, under Finance.
	got := check()
	if n, ok := got["j-py"]; !ok || n.AgencyID != "ag-fin" || !strings.Contains(n.Detail, "no scope (its runs are its agency's)") {
		t.Fatalf("a requirement no Finance agent advertises: notices %v; want one under Finance", got)
	}
	// It does now: nothing to report (until CA6 Global's agents were asked).
	mustExec(t, pool, `UPDATE runners SET capabilities = '["bash","python"]' WHERE id = 'rn-fin'`)
	if got := check(); len(got) != 0 {
		t.Errorf("with a Finance agent advertising python: notices %v, want none", got)
	}
}
