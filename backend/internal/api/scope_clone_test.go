package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A scope is cloned to an agency (2.4.0 Phase R6; the owner's decision of
// 2026-10-13): a new scope built in the app, the target agency's, under a name
// of its own, with the original's hosts, groups, nesting, variables and tags,
// and its host records without their keys. Not its runner bindings, and not
// its jobs. It is a snapshot. Who may: who may move the scope to that agency.
func TestAScopeIsClonedToAnAgency(t *testing.T) {
	h, pool := gateServer(t) // sc:fin "fin-hosts" is FIN's; sc:tax "tax-hosts" is TAX's
	exec := mustExec(t, pool)
	exec(`UPDATE scopes SET description = 'Finance''s hosts', tags = '["prod","pci"]', raw_inventory = '[web]' || char(10) || 'h1', inventory_format = 'ini' WHERE id = 'sc:fin'`)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:fin', 'h1'), ('sc:fin', 'h2')`)
	exec(`INSERT INTO scope_groups (scope_id, name) VALUES ('sc:fin', 'web'), ('sc:fin', 'site')`)
	exec(`INSERT INTO scope_group_hosts (scope_id, group_name, host) VALUES ('sc:fin', 'web', 'h1')`)
	exec(`INSERT INTO scope_group_children (scope_id, parent, child) VALUES ('sc:fin', 'site', 'web')`)
	exec(`INSERT INTO scope_host_vars (scope_id, host, key, value) VALUES ('sc:fin', 'h1', 'ansible_port', '2222')`)
	exec(`INSERT INTO scope_group_vars (scope_id, group_name, key, value) VALUES ('sc:fin', 'web', 'tier', 'front'), ('sc:fin', 'site', 'dc', 'east')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, port, username, address, os, auth_key_env_var, source, scope_id, owner_agency, created_at) VALUES
	        ('rec-h1', 'h1', 2222, 'deploy', '10.0.0.1', 'Linux', 'FIN_DEPLOY_KEY', 'cronomicon', 'sc:fin', 'ag:FIN', 't'),
	        ('rec-h2', 'h2', 22,   'deploy', '10.0.0.2', 'Linux', 'FIN_DEPLOY_KEY', 'cronomicon', 'sc:fin', 'ag:FIN', 't')`)
	exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
	        VALUES ('agent-fin', 'agent-fin', 'agent', 'offline', 'Linux', '["bash"]', 0, 5, '1.0', 't', 't')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc:fin', 'agent-fin', 'agent-fin', 't')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, enabled, created_at) VALUES ('j-fin', 'fin-job', 'cronomicon', 'bash', 'fin-hosts', 1, 't')`)
	one := func(q string, args ...any) string {
		t.Helper()
		var v sql.NullString
		if err := pool.QueryRow(q, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		return v.String
	}
	clone := func(who, scope, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/scopes/"+scope+"/clone", who, body)
		return rec.Code, rec.Body.String()
	}
	const both = gFinAdmin + "," + gTaxAdmin
	const toTax = `{"agencyId":"ag:TAX","name":"tax-copy-of-fin"}`

	// Who may not: an administrator of one side alone, and anyone who
	// administers neither.
	for _, who := range []string{gFinAdmin, gTaxAdmin, gFinApprover, gAllViewer} {
		if code, _ := clone(who, "sc:fin", toTax); code != http.StatusForbidden {
			t.Errorf("%s cloning FIN's scope to TAX = %d, want 403", who, code)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scopes`); n != 3 {
		t.Fatalf("a refused clone made a scope: %d scopes", n)
	}

	// An administrator of both.
	code, body := clone(both, "sc:fin", toTax)
	if code != http.StatusCreated {
		t.Fatalf("an administrator of both agencies cloning FIN's scope to TAX = %d %s, want 201", code, body)
	}
	var answer struct {
		Copied struct{ Hosts, Groups, HostVars, GroupVars, HostRecords int }
		Note   string
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Copied.Hosts != 2 || answer.Copied.Groups != 2 || answer.Copied.HostVars != 1 || answer.Copied.GroupVars != 2 || answer.Copied.HostRecords != 2 {
		t.Errorf("the answer says it copied %+v", answer.Copied)
	}
	if !strings.Contains(answer.Note, "without their SSH keys") {
		t.Errorf("the answer does not say the keys were left unset: %q", answer.Note)
	}
	id := one(`SELECT id FROM scopes WHERE name = 'tax-copy-of-fin'`)

	// A scope of its own, built in the app, the target agency's alone.
	if got := one(`SELECT source || '|' || COALESCE(repo_id, '') || '|' || COALESCE(description, '') || '|' || tags || '|' || COALESCE(raw_inventory, '') FROM scopes WHERE id = ?`, id); got != "cronomicon||Finance's hosts|[\"prod\",\"pci\"]|[web]\nh1" {
		t.Errorf("the clone's row: %q", got)
	}
	if got := one(`SELECT GROUP_CONCAT(agency_id) FROM scope_agencies WHERE scope_id = ?`, id); got != "ag:TAX" {
		t.Errorf("the clone is in %q, want TAX alone", got)
	}
	// Its contents are the original's.
	for what, q := range map[string]string{
		"hosts":           `SELECT GROUP_CONCAT(host, ',') FROM (SELECT host FROM scope_hosts WHERE scope_id = ? ORDER BY host)`,
		"groups":          `SELECT GROUP_CONCAT(name, ',') FROM (SELECT name FROM scope_groups WHERE scope_id = ? ORDER BY name)`,
		"group hosts":     `SELECT GROUP_CONCAT(group_name || ':' || host, ',') FROM scope_group_hosts WHERE scope_id = ?`,
		"nesting":         `SELECT GROUP_CONCAT(parent || '>' || child, ',') FROM scope_group_children WHERE scope_id = ?`,
		"host variables":  `SELECT GROUP_CONCAT(host || '.' || key || '=' || value, ',') FROM scope_host_vars WHERE scope_id = ?`,
		"group variables": `SELECT GROUP_CONCAT(v, ',') FROM (SELECT group_name || '.' || key || '=' || value AS v FROM scope_group_vars WHERE scope_id = ? ORDER BY 1)`,
	} {
		if got, want := one(q, id), one(q, "sc:fin"); got != want || got == "" {
			t.Errorf("the clone's %s are %q, the original's %q", what, got, want)
		}
	}
	// Its host records: where and as whom, the target agency's, with no key.
	if got := one(`SELECT GROUP_CONCAT(r, ';') FROM (SELECT hostname || '|' || port || '|' || username || '|' || address || '|' || os || '|' || owner_agency || '|' || source
	                  || '|' || COALESCE(auth_key_env_var, '') || '|' || COALESCE(auth_credential_id, '') || '|' || COALESCE(bastion_id, '') AS r
	                  FROM ssh_hosts WHERE scope_id = ? ORDER BY hostname)`, id); got != "h1|2222|deploy|10.0.0.1|Linux|ag:TAX|cronomicon|||;h2|22|deploy|10.0.0.2|Linux|ag:TAX|cronomicon|||" {
		t.Errorf("the clone's host records: %q", got)
	}
	// Not its runners, and not its jobs.
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE scope_id = ?`, id); n != 0 {
		t.Errorf("the clone has %d runner bindings, want none", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM jobs WHERE scope = 'tax-copy-of-fin'`); n != 0 {
		t.Errorf("the clone has %d jobs, want none", n)
	}
	// The original is as it was, and still its agency's.
	if got := one(`SELECT GROUP_CONCAT(agency_id) FROM scope_agencies WHERE scope_id = 'sc:fin'`); got != "ag:FIN" {
		t.Errorf("the original is in %q after the clone", got)
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc:fin') + (SELECT COUNT(*) FROM scope_runners WHERE scope_id = 'sc:fin')
	                         + (SELECT COUNT(*) FROM ssh_hosts WHERE scope_id = 'sc:fin' AND auth_key_env_var = 'FIN_DEPLOY_KEY') + (SELECT COUNT(*) FROM jobs WHERE scope = 'fin-hosts')`); n != 6 {
		t.Errorf("the original's hosts, binding, records and job after the clone: %d, want 6", n)
	}
	// A snapshot: what is added to the original afterwards is not the clone's.
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:fin', 'h3')`)
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_hosts WHERE scope_id = ?`, id); n != 2 {
		t.Errorf("the clone has %d hosts after one was added to the original, want its 2", n)
	}

	// The name is its own in the target agency (I2): one TAX has is refused,
	// one FIN has (the original's) is TAX's to take, and one is required.
	if code, _ := clone(both, "sc:fin", toTax); code != http.StatusConflict {
		t.Errorf("a second clone under the same name = %d, want 409", code)
	}
	if code, body := clone(both, "sc:fin", `{"agencyId":"ag:TAX","name":"fin-hosts"}`); code != http.StatusCreated {
		t.Errorf("a clone under the original's name, into another agency = %d %s, want 201 (names are per agency)", code, body)
	}
	if code, _ := clone(both, "sc:fin", `{"agencyId":"ag:TAX","name":"  "}`); code != http.StatusUnprocessableEntity {
		t.Errorf("a clone with no name = %d, want 422", code)
	}
	// A global administrator: any pair, and is told what is not there.
	if code, body := clone(gRoot, "sc:tax", `{"agencyId":"ag:FIN","name":"fin-copy-of-tax"}`); code != http.StatusCreated {
		t.Errorf("a global administrator cloning TAX's scope to FIN = %d %s", code, body)
	}
	if code, _ := clone(gRoot, "sc:nope", `{"agencyId":"ag:FIN","name":"x"}`); code != http.StatusNotFound {
		t.Errorf("a global administrator cloning a scope that is not there = %d, want 404", code)
	}
	if code, body := clone(gRoot, "sc:fin", `{"agencyId":"ag:NOPE","name":"y"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "unknown_agency") {
		t.Errorf("a clone to an agency that is not there = %d %s, want 422 unknown_agency", code, body)
	}
	// A scope that comes from Git is cloned the same way, and the clone is
	// built in the app: Git goes on owning the original.
	exec(`INSERT INTO scopes (id, name, source, source_path, created_at, synced_at) VALUES ('sc:git', 'git-hosts', 'git', 'inventory/git-hosts.ini', 't', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc:git'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc:git', 'ag:FIN')`)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:git', 'g1')`)
	if code, body := clone(both, "sc:git", `{"agencyId":"ag:TAX","name":"tax-copy-of-git"}`); code != http.StatusCreated {
		t.Fatalf("cloning a scope that comes from Git = %d %s", code, body)
	}
	if got := one(`SELECT source || '|' || COALESCE(source_path, '') || '|' || (SELECT COUNT(*) FROM scope_hosts h WHERE h.scope_id = scopes.id) FROM scopes WHERE name = 'tax-copy-of-git'`); got != "cronomicon||1" {
		t.Errorf("the clone of a Git scope: %q, want a scope built in the app with its one host", got)
	}
	if got := one(`SELECT source FROM scopes WHERE id = 'sc:git'`); got != "git" {
		t.Errorf("the original is %q after the clone, want it Git's still", got)
	}
}
