package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An agency's job that binds a name only Global holds is reported to the
// agency (§0a, IN-6, IN-14): Global's row is Global's runs' only now. A row
// Global owns that was shared into the agency counts as Global's. It clears
// when the agency has its own row of the name. Not reported: Global's own
// job, and a name the agency holds.
func TestI1_AJobBindingOnlyGlobalsSecretIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-tax', 'tax-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-tax', 'ag-tax')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES
	                   ('j-tax', 'tax-job', 'cronomicon', 'bash', 'tax-hosts', 't'), ('j-g', 'g-job', 'cronomicon', 'bash', NULL, 't')`)
	mustExec(t, pool, `INSERT INTO secrets (id, key, source, created_at, owner_agency) VALUES
	                   ('s-g', 'TOKEN', 'stored', 't', 'global'), ('s-own', 'OWN', 'stored', 't', 'ag-tax'), ('s-g2', 'OWN', 'stored', 't', 'global')`)
	bind := func(uid, job, name string) {
		mustExec(t, pool, `INSERT INTO reference_bindings (owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at, owner_uid)
		                   VALUES ('job', 'cronomicon', ?, 'secret', ?, 't', ?)`, job, name, uid)
	}
	bind("j-tax", "tax-job", "TOKEN")
	bind("j-tax", "tax-job", "OWN")
	bind("j-g", "g-job", "TOKEN")

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindGlobalCredentialBound)
	}
	got := check()
	n, ok := got["j-tax:secret:TOKEN"]
	if len(got) != 1 || !ok {
		t.Fatalf("open notices = %v; want Tax's job's binding of TOKEN alone", got)
	}
	if n.AgencyID != "ag-tax" || !strings.Contains(n.Detail, "tax-job") || !strings.Contains(n.Detail, "only Global has") {
		t.Errorf("the notice is filed under %q with %q", n.AgencyID, n.Detail)
	}
	mustExec(t, pool, `INSERT INTO secrets (id, key, source, created_at, owner_agency) VALUES ('s-tax', 'TOKEN', 'stored', 't', 'ag-tax')`)
	if got := check(); len(got) != 0 {
		t.Errorf("after Tax got its own TOKEN: %v", got)
	}
}

// An agency's job built in the app on a script of Global's repository is
// reported to the agency (§0a; the owner's yes to question 31), and the notice
// clears when the job is built on a script of its own. Global's own job on
// Global's script is not reported.
func TestI1_AnAgencysJobOnGlobalsScriptIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-tax', 'tax-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-tax', 'ag-tax')`)
	mustExec(t, pool, `INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-tax', 'ag-tax', 'u', 'main')`)
	mustExec(t, pool, `INSERT INTO scripts (uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES
	                   ('s-g', 'global', 'run.sh', 'bash', 'true', 'h', 't'), ('s-tax', 'repo-tax', 'run.sh', 'bash', 'true', 'h', 't')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, script_uid, synced_at) VALUES
	                   ('j-tax', 'tax-job', 'cronomicon', 'bash', 'tax-hosts', 's-g', 't'), ('j-g', 'g-job', 'cronomicon', 'bash', NULL, 's-g', 't')`)
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindGlobalScriptJob)
	}
	got := check()
	if n, ok := got["j-tax"]; len(got) != 1 || !ok || n.AgencyID != "ag-tax" || !strings.Contains(n.Detail, "tax-job") {
		t.Fatalf("open notices = %v; want Tax's job alone, filed under Tax", got)
	}
	mustExec(t, pool, `UPDATE jobs SET script_uid = 's-tax' WHERE uid = 'j-tax'`)
	if got := check(); len(got) != 0 {
		t.Errorf("after Tax's job was built on Tax's script: %v", got)
	}
}

// An agency the local runner served until 2.4.0 is told its shell jobs wait
// for an agent of its own (§0a, IN-3), until one serves it.
func TestI1_AnAgencyTheLocalRunnerServedIsTold(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't'), ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO settings (key, value) VALUES (?, '["ag-tax","ag-gone"]')`, notices.LocalRunnerServedKey)
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindLocalRunnerServed)
	}
	got := check()
	if n, ok := got["ag-tax"]; len(got) != 1 || !ok || n.AgencyID != "ag-tax" || !strings.Contains(n.Detail, "Tax") {
		t.Fatalf("open notices = %v; want one for Tax, filed under Tax", got)
	}
	mustExec(t, pool, `INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
	                   VALUES ('r-tax', 'tax-agent', 'agent', 'online', 'Linux', '["bash"]', 0, 1, '1', 't', 't', 'ag-tax')`)
	mustExec(t, pool, `INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES ('r-tax', 'ag-tax')`)
	if got := check(); len(got) != 0 {
		t.Errorf("after Tax got an agent of its own: %v", got)
	}
}
