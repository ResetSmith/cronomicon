package api_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The agency switcher (2.4.4). Every list the switcher narrows, with a row of
// FIN's, a row of TAX's and a row of Global's in each, so that what each kind
// of person sees can be told apart: S0 pins what each list shows today, with
// no agency named, and S1 shows that naming an agency narrows that and never
// widens it.
//
// Rows are named s0-<fin|tax|glob>-<kind>, so a list's body can be searched for
// each agency's marker. Scopes and grants keep the gate fixture's own names.

// seedSwitcher adds the rows. The gate fixture (gateServer) already holds the
// two agencies, their scopes and the grants.
func seedSwitcher(t *testing.T, pool *sql.DB) {
	t.Helper()
	exec := mustExec(t, pool)
	const at = "2026-01-01T00:00:00Z"

	exec(`UPDATE scopes SET owner_agency = 'ag:FIN' WHERE id = 'sc:fin'`)
	exec(`UPDATE scopes SET owner_agency = 'ag:TAX' WHERE id = 'sc:tax'`)

	// Jobs: one per agency on its scope, Global's with none, and FIN's with none
	// (the no-scope case the scope filter admits for everyone).
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, created_at) VALUES
	        ('j-fin',  's0-fin-job',     'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', ?),
	        ('j-tax',  's0-tax-job',     'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', ?),
	        ('j-glob', 's0-glob-job',    'cronomicon', 'bash', NULL,        NULL,     'true', ?),
	        ('j-fin0', 's0-fin-noscope', 'cronomicon', 'bash', NULL,        'ag:FIN', 'true', ?)`, at, at, at, at)
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, scope, agencies_json) VALUES
	        ('r-fin',  's0-fin-job',     'bash', 'success', 'x', 'manual', ?, 'fin-hosts', '["FIN"]'),
	        ('r-tax',  's0-tax-job',     'bash', 'success', 'x', 'manual', ?, 'tax-hosts', '["TAX"]'),
	        ('r-glob', 's0-glob-job',    'bash', 'success', 'x', 'manual', ?, NULL,        '[]'),
	        ('r-fin0', 's0-fin-noscope', 'bash', 'success', 'x', 'manual', ?, NULL,        '[]')`, at, at, at, at)

	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-fin',  's0-fin-flow',  'cronomicon', 'ag:FIN', '[]', ?),
	        ('w-tax',  's0-tax-flow',  'cronomicon', 'ag:TAX', '[]', ?),
	        ('w-glob', 's0-glob-flow', 'cronomicon', 'global', '[]', ?)`, at, at, at)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at) VALUES
	        ('wr-fin',  's0-fin-flow',  'cronomicon', 'w-fin',  'success', 'x', 'manual', ?),
	        ('wr-tax',  's0-tax-flow',  'cronomicon', 'w-tax',  'success', 'x', 'manual', ?),
	        ('wr-glob', 's0-glob-flow', 'cronomicon', 'w-glob', 'success', 'x', 'manual', ?)`, at, at, at)

	exec(`INSERT INTO schedules (uid, name, source, owner_agency, cron, content_hash, created_at) VALUES
	        ('sch-fin',  's0-fin-sched',  'cronomicon', 'ag:FIN', '0 1 * * *', 'h', ?),
	        ('sch-tax',  's0-tax-sched',  'cronomicon', 'ag:TAX', '0 2 * * *', 'h', ?),
	        ('sch-glob', 's0-glob-sched', 'cronomicon', 'global', '0 3 * * *', 'h', ?)`, at, at, at)

	// The credential tables' lists scan their audit columns as text.
	audit := func(table string) {
		exec(`UPDATE ` + table + ` SET created_by = 'x', last_modified_by = 'x', last_modified_at = '` + at + `'`)
	}
	defer func() {
		for _, table := range []string{"secrets", "env_vars", "ssh_credentials", "ssh_hosts", "bastions"} {
			audit(table)
		}
		exec(`UPDATE secrets SET scope = '', description = '' WHERE id LIKE 'sec-%'`)
		exec(`UPDATE env_vars SET scope = '', description = '' WHERE id LIKE 'ev-%'`)
		// A second pair bound to each agency's scope, beside the agency-wide pair.
		exec(`INSERT INTO secrets (id, key, source, vault_ref, scope, description, owner_agency, created_by, created_at, last_modified_by, last_modified_at) VALUES
		        ('sec-fin-s', 'S0_FIN_SCOPED_SECRET', 'vault', 'kv/fs', 'fin-hosts', '', 'ag:FIN', 'x', ?, 'x', ?),
		        ('sec-tax-s', 'S0_TAX_SCOPED_SECRET', 'vault', 'kv/ts', 'tax-hosts', '', 'ag:TAX', 'x', ?, 'x', ?)`, at, at, at, at)
		exec(`INSERT INTO env_vars (id, key, value, scope, description, owner_agency, created_by, created_at, last_modified_by, last_modified_at) VALUES
		        ('ev-fin-s', 'S0_FIN_SCOPED_VAR', '1', 'fin-hosts', '', 'ag:FIN', 'x', ?, 'x', ?),
		        ('ev-tax-s', 'S0_TAX_SCOPED_VAR', '1', 'tax-hosts', '', 'ag:TAX', 'x', ?, 'x', ?)`, at, at, at, at)
		exec(`UPDATE ssh_credentials SET description = '' WHERE id LIKE 'k-%'`)
	}()

	exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES
	        ('rn-fin',  's0-fin-runner',  'online', ?, ?, 'ag:FIN'),
	        ('rn-tax',  's0-tax-runner',  'online', ?, ?, 'ag:TAX'),
	        ('rn-glob', 's0-glob-runner', 'online', ?, ?, 'global')`, at, at, at, at, at, at)

	exec(`INSERT INTO secrets (id, key, source, vault_ref, owner_agency, created_at) VALUES
	        ('sec-fin',  'S0_FIN_SECRET',  'vault', 'kv/fin',  'ag:FIN', ?),
	        ('sec-tax',  'S0_TAX_SECRET',  'vault', 'kv/tax',  'ag:TAX', ?),
	        ('sec-glob', 'S0_GLOB_SECRET', 'vault', 'kv/glob', '',       ?)`, at, at, at)
	exec(`INSERT INTO env_vars (id, key, value, owner_agency, created_at) VALUES
	        ('ev-fin',  'S0_FIN_VAR',  '1', 'ag:FIN', ?),
	        ('ev-tax',  'S0_TAX_VAR',  '1', 'ag:TAX', ?),
	        ('ev-glob', 'S0_GLOB_VAR', '1', '',       ?)`, at, at, at)
	exec(`INSERT INTO ssh_credentials (id, label, source, vault_ref, owner_agency, created_at) VALUES
	        ('k-fin',  's0-fin-key',  'vault', 'kv/k1', 'ag:FIN', ?),
	        ('k-tax',  's0-tax-key',  'vault', 'kv/k2', 'ag:TAX', ?),
	        ('k-glob', 's0-glob-key', 'vault', 'kv/k3', '',       ?)`, at, at, at)
	exec(`INSERT INTO ssh_hosts (id, hostname, owner_agency, created_at) VALUES
	        ('h-fin',  's0-fin-host',  'ag:FIN', ?),
	        ('h-tax',  's0-tax-host',  'ag:TAX', ?),
	        ('h-glob', 's0-glob-host', 'global', ?)`, at, at, at)
	exec(`INSERT INTO bastions (id, hostname, name, owner_agency, created_at) VALUES
	        ('b-fin',  's0-fin-bastion',  's0-fin-bastion',  'ag:FIN', ?),
	        ('b-tax',  's0-tax-bastion',  's0-tax-bastion',  'ag:TAX', ?),
	        ('b-glob', 's0-glob-bastion', 's0-glob-bastion', 'global', ?)`, at, at, at)
	exec(`INSERT INTO calendars (name, source, owner_agency, created_at) VALUES
	        ('s0-fin-cal',  'cronomicon', 'ag:FIN', ?),
	        ('s0-tax-cal',  'cronomicon', 'ag:TAX', ?),
	        ('s0-glob-cal', 'cronomicon', 'global', ?)`, at, at, at)
	exec(`INSERT INTO notices (id, kind, agency_id, subject, detail, first_seen_at, last_seen_at) VALUES
	        ('n-fin',  's0', 'ag:FIN', 's0-fin-notice',  '', ?, ?),
	        ('n-tax',  's0', 'ag:TAX', 's0-tax-notice',  '', ?, ?),
	        ('n-glob', 's0', 'global', 's0-glob-notice', '', ?, ?)`, at, at, at, at, at, at)

	exec(`INSERT INTO git_repos (id, agency_id, url, created_at) VALUES
	        ('repo-fin', 'ag:FIN', 'http://127.0.0.1:1/s0-fin-repo.git', ?),
	        ('repo-tax', 'ag:TAX', 'http://127.0.0.1:1/s0-tax-repo.git', ?)`, at, at)
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES
	        ('sx-fin',  'repo-fin', 's0-fin-script.sh',  'bash', 'true', 'h', ?),
	        ('sx-tax',  'repo-tax', 's0-tax-script.sh',  'bash', 'true', 'h', ?),
	        ('sx-glob', 'global',   's0-glob-script.sh', 'bash', 'true', 'h', ?)`, at, at, at)

	exec(`INSERT INTO service_accounts (id, name, token_hash, role, created_by, created_at, agency_id, all_scopes) VALUES
	        ('sa-fin',  's0-fin-sa',  'h1', 'viewer', 'x', ?, 'ag:FIN', 0),
	        ('sa-tax',  's0-tax-sa',  'h2', 'viewer', 'x', ?, 'ag:TAX', 0),
	        ('sa-glob', 's0-glob-sa', 'h3', 'viewer', 'x', ?, NULL,     1)`, at, at, at)
}

// switcherList is one list the switcher narrows, and the marker of each
// agency's row in its body.
type switcherList struct {
	path          string
	fin, tax, glb string
}

var switcherLists = []switcherList{
	{"/api/v1/jobs?pageSize=200", "s0-fin-job", "s0-tax-job", "s0-glob-job"},
	{"/api/v1/runs?pageSize=200", `"s0-fin-job"`, `"s0-tax-job"`, `"s0-glob-job"`},
	// FIN's job with no scope, and its run (the scope filter admits a row with no scope).
	{"/api/v1/jobs?pageSize=200&noscope=1", "s0-fin-noscope", "s0-tax-job", "s0-glob-job"},
	{"/api/v1/runs?pageSize=200&noscope=1", `"s0-fin-noscope"`, `"s0-tax-job"`, `"s0-glob-job"`},
	{"/api/v1/workflow-runs?pageSize=200", "s0-fin-flow", "s0-tax-flow", "s0-glob-flow"},
	{"/api/v1/workflows?pageSize=200", "s0-fin-flow", "s0-tax-flow", "s0-glob-flow"},
	{"/api/v1/schedule-defs?pageSize=200", "s0-fin-sched", "s0-tax-sched", "s0-glob-sched"},
	{"/api/v1/scopes", `"fin-hosts"`, `"tax-hosts"`, `"shared"`},
	{"/api/v1/runners", "s0-fin-runner", "s0-tax-runner", "s0-glob-runner"},
	{"/api/v1/env-secrets", "S0_FIN_SECRET", "S0_TAX_SECRET", "S0_GLOB_SECRET"},
	{"/api/v1/env-vars", "S0_FIN_VAR", "S0_TAX_VAR", "S0_GLOB_VAR"},
	{"/api/v1/env-secrets?scoped=1", "S0_FIN_SCOPED_SECRET", "S0_TAX_SCOPED_SECRET", "S0_GLOB_SECRET"},
	{"/api/v1/env-vars?scoped=1", "S0_FIN_SCOPED_VAR", "S0_TAX_SCOPED_VAR", "S0_GLOB_VAR"},
	{"/api/v1/ssh/credentials", "s0-fin-key", "s0-tax-key", "s0-glob-key"},
	{"/api/v1/ssh/hosts", "s0-fin-host", "s0-tax-host", "s0-glob-host"},
	{"/api/v1/ssh/bastions", "s0-fin-bastion", "s0-tax-bastion", "s0-glob-bastion"},
	{"/api/v1/calendars", "s0-fin-cal", "s0-tax-cal", "s0-glob-cal"},
	{"/api/v1/notices", "s0-fin-notice", "s0-tax-notice", "s0-glob-notice"},
	{"/api/v1/git-repo-names", `"repo-fin"`, `"repo-tax"`, `"global"`},
	{"/api/v1/git-repos", "s0-fin-repo", "s0-tax-repo", `"global"`},
	{"/api/v1/scripts?pageSize=200", "s0-fin-script", "s0-tax-script", "s0-glob-script"},
	{"/api/v1/access-grants", `"fin-admins"`, `"tax-admins"`, `"root-admins"`},
	{"/api/v1/service-accounts", "s0-fin-sa", "s0-tax-sa", "s0-glob-sa"},
}

// The kinds of person, by the groups they present.
var switcherPeople = []struct{ name, groups string }{
	{"root", gRoot},
	{"all-viewer", gAllViewer},
	{"fin-admin", gFinAdmin},
	{"tax-admin", gTaxAdmin},
	{"fin+tax admin", gFinAdmin + "," + gTaxAdmin},
	{"fin-viewer", gFinViewer},
}

// switcherToday is what each list shows today, with no agency named, to each
// kind of person in switcherPeople's order: for each, which of FIN's, TAX's
// and Global's rows it lists ("Y" listed, "." not), or the status when the list
// is refused. Pinned in 2.4.4 S0 so that S1 can show the switcher narrows it
// and never widens it.
//
// Several rows here are read gaps the survey of 2026-10-16 found, kept as they
// are (2.5.0's confidential reads, CF-2; plan-2.4.4 §7): workflows, workflow
// runs, schedule definitions, scopes, runners, agency-wide secrets and
// variables, SSH keys, hosts, bastions and calendars list every agency's rows
// to everyone; FIN's job with no scope, and its run, are listed to TAX; any
// agency's administrator lists every agency's grants.
var switcherToday = map[string][6]string{
	"/api/v1/jobs?pageSize=200":           {"YYY", "YYY", "Y.Y", ".YY", "YYY", "Y.Y"},
	"/api/v1/runs?pageSize=200":           {"YYY", "YYY", "Y.Y", ".YY", "YYY", "Y.Y"},
	"/api/v1/jobs?pageSize=200&noscope=1": {"YYY", "YYY", "Y.Y", "YYY", "YYY", "Y.Y"},
	"/api/v1/runs?pageSize=200&noscope=1": {"YYY", "YYY", "Y.Y", "YYY", "YYY", "Y.Y"},
	"/api/v1/workflow-runs?pageSize=200":  {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/workflows?pageSize=200":      {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/schedule-defs?pageSize=200":  {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/scopes":                      {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/runners":                     {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/env-secrets":                 {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/env-vars":                    {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/env-secrets?scoped=1":        {"YYY", "YYY", "Y.Y", ".YY", "YYY", "Y.Y"},
	"/api/v1/env-vars?scoped=1":           {"YYY", "YYY", "Y.Y", ".YY", "YYY", "Y.Y"},
	"/api/v1/ssh/credentials":             {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/ssh/hosts":                   {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/ssh/bastions":                {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/calendars":                   {"YYY", "YYY", "YYY", "YYY", "YYY", "YYY"},
	"/api/v1/notices":                     {"YYY", "...", "Y..", ".Y.", "YY.", "..."},
	"/api/v1/git-repo-names":              {"YYY", "YYY", "Y.Y", ".YY", "YYY", "Y.Y"},
	"/api/v1/git-repos":                   {"YYY", "403", "Y..", ".Y.", "YY.", "403"},
	"/api/v1/scripts?pageSize=200":        {"YYY", "YYY", "Y.Y", ".YY", "YYY", "Y.Y"},
	"/api/v1/access-grants":               {"YYY", "403", "YYY", "YYY", "YYY", "403"},
	"/api/v1/service-accounts":            {"YYY", "403", "Y..", ".Y.", "YY.", "403"},
}

// switcherSees asks one list as one person and answers as switcherToday does.
func switcherSees(t *testing.T, h http.Handler, l switcherList, groups string, header ...string) string {
	t.Helper()
	var rec *httptest.ResponseRecorder
	if len(header) == 2 {
		rec = gateReqWithHeader(t, h, http.MethodGet, l.path, groups, "", header[0], header[1])
	} else {
		rec = gateReq(t, h, http.MethodGet, l.path, groups, "")
	}
	if rec.Code != http.StatusOK {
		return strconv.Itoa(rec.Code)
	}
	b := rec.Body.String()
	out := ""
	for _, m := range []string{l.fin, l.tax, l.glb} {
		if strings.Contains(b, m) {
			out += "Y"
		} else {
			out += "."
		}
	}
	return out
}

// With no agency named, every list answers as it did before 2.4.4.
func TestSwitcherListsWithNoAgencyNamedAnswerAsBefore(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	if len(switcherToday) != len(switcherLists) {
		t.Fatalf("switcherToday pins %d lists, switcherLists has %d", len(switcherToday), len(switcherLists))
	}
	for _, l := range switcherLists {
		want, ok := switcherToday[l.path]
		if !ok {
			t.Errorf("%s: not pinned in switcherToday", l.path)
			continue
		}
		for i, p := range switcherPeople {
			if got := switcherSees(t, h, l, p.groups); got != want[i] {
				t.Errorf("%s as %s: %s, want %s (FIN, TAX, Global)", l.path, p.name, got, want[i])
			}
		}
	}
}
