package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// GR-16 (2.4.0) where the APPLICATION resolves a name: a definition built in
// the app looks a Git name up in its own agency's repository, then in Global's,
// and never in another agency's.

// A reaction written in the app pins the upstream the engine would find by the
// name. Until Phase R4 it pinned "the one Git definition of that name, wherever
// it is", which with a repository per agency can be another agency's job.
//
// And a name that only ANOTHER agency's repository holds is no upstream at all
// for this owner: the save is refused, in the words used for a name nobody
// holds. (As Phase R4 was first built it was saved with no upstream, would
// never fire, and was listed as healthy.)
func TestAReactionWrittenInTheAppPinsItsAgencysRepositorysDefinition(t *testing.T) {
	api, pool := newRxAPI(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main'), ('repo-c', 'ag-c', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-b', 'b-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-b'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-b', 'ag-b')`)
	// Who reacts: a job built in the app in agency B, and one that is Global's.
	exec(`INSERT INTO jobs (name, uid, source, run_type, enabled, synced_at, scope) VALUES
	      ('load-b', 'uid-load-b', 'cronomicon', 'bash', 1, 't', 'b-hosts'),
	      ('load-g', 'uid-load-g', 'cronomicon', 'bash', 1, 't', NULL)`)
	gitJob := func(uid, repo string) {
		t.Helper()
		exec(`INSERT INTO jobs (name, uid, source, run_type, enabled, synced_at, repo_id) VALUES ('nightly', ?, 'git', 'bash', 1, 't', ?)`, uid, repo)
	}
	try := func(owner, upstream string) (int, string) {
		t.Helper()
		return api.doRaw(http.MethodPut, "/api/v1/reactions/job/"+owner, map[string]any{
			"reactions": []map[string]any{{"name": "after-it", "onKind": "job", "onName": upstream, "onSource": "git", "onOutcome": "success"}},
		})
	}
	put := func(owner string) {
		t.Helper()
		if code, body := try(owner, "nightly"); code != http.StatusOK {
			t.Fatalf("PUT reactions of %s = %d (%s)", owner, code, body)
		}
	}
	pinned := func(owner string) string {
		t.Helper()
		var uid string
		if err := pool.QueryRow(`SELECT COALESCE(on_uid, '(none)') FROM reactions WHERE owner_name = ? AND owner_source = 'cronomicon'`, owner).Scan(&uid); err != nil {
			t.Fatalf("no reaction row for %s: %v", owner, err)
		}
		return uid
	}

	// Only ANOTHER agency's repository has a nightly: for these owners there is
	// no such job, and the answer is the one a name nobody holds gets.
	gitJob("n-c", "repo-c")
	for _, owner := range []string{"load-b", "load-g"} {
		code, body := try(owner, "nightly")
		nobodyCode, nobodyBody := try(owner, "no-such-job")
		if code != http.StatusUnprocessableEntity || code != nobodyCode ||
			strings.ReplaceAll(body, "nightly", "X") != strings.ReplaceAll(nobodyBody, "no-such-job", "X") {
			t.Errorf("%s reacting to a job only another agency's repository has: %d %s; want what a name nobody holds gets: %d %s",
				owner, code, body, nobodyCode, nobodyBody)
		}
		if n := count(t, pool, `SELECT COUNT(*) FROM reactions WHERE owner_name = ?`, owner); n != 0 {
			t.Errorf("a refused save left %d reaction row(s) for %s", n, owner)
		}
	}
	// Global's repository gets one: both mean it.
	gitJob("n-g", "global")
	put("load-b")
	put("load-g")
	for _, owner := range []string{"load-b", "load-g"} {
		if got := pinned(owner); got != "n-g" {
			t.Errorf("with Global's nightly, %s's reaction is pinned to %q; want Global's", owner, got)
		}
	}
	// Agency B's repository gets its own: B's job means that one, Global's job Global's.
	gitJob("n-b", "repo-b")
	put("load-b")
	put("load-g")
	if got := pinned("load-b"); got != "n-b" {
		t.Errorf("with its own agency's nightly, load-b's reaction is pinned to %q; want its agency's repository's", got)
	}
	if got := pinned("load-g"); got != "n-g" {
		t.Errorf("Global's job's reaction is pinned to %q; want Global's nightly", got)
	}
}

// The job composer takes a script, and a Git schedule, from the repository of
// the job's agency, then from Global's, and not from another agency's.
// Between a schedule built in the app and Git's of the same name the app's
// still comes first, as it always has.
func TestTheComposerResolvesAScriptAndAScheduleInItsAgencysRepository(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'u', 'main'), ('repo-tax', 'ag:TAX', 'u', 'main')`)
	script := func(uid, repo, name, body string) {
		t.Helper()
		exec(`INSERT INTO scripts (uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES (?, ?, ?, 'bash', ?, ?, 't')`,
			uid, repo, name, body, "hash-"+uid)
	}
	schedule := func(uid, source, name, cron string, repo, owner any) {
		t.Helper()
		exec(`INSERT INTO schedules (uid, name, source, cron, content_hash, repo_id, owner_agency) VALUES (?, ?, ?, ?, 'h', ?, ?)`,
			uid, name, source, cron, repo, owner)
	}
	// One script name in three repositories; one only in Global's; one only in TAX's.
	script("s-tax", "repo-tax", "deploy.sh", "echo from-tax")
	script("s-glob", "global", "deploy.sh", "echo from-global")
	script("s-fin", "repo-fin", "deploy.sh", "echo from-fin")
	script("s-shared", "global", "shared.sh", "echo shared")
	script("s-taxonly", "repo-tax", "tax-only.sh", "echo tax-only")
	schedule("d-tax", "git", "nightly", "0 0 1 * * *", "repo-tax", "ag:TAX")
	schedule("d-glob", "git", "nightly", "0 0 2 * * *", "global", "global")
	schedule("d-fin", "git", "nightly", "0 0 3 * * *", "repo-fin", "ag:FIN")
	schedule("d-shared", "git", "weekly", "0 0 4 * * 1", "global", "global")
	schedule("d-taxonly", "git", "tax-only", "0 0 5 * * *", "repo-tax", "ag:TAX")
	schedule("d-both-git", "git", "both", "0 0 6 * * *", "repo-fin", "ag:FIN")
	schedule("d-both-app", "cronomicon", "both", "0 0 7 * * *", nil, "global")

	compose := func(who, name, scope, scriptRef, scheduleRef string) int {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"scope":%q,"scriptRef":%q,"scheduleRefs":[%q]}`, name, scope, scriptRef, scheduleRef)
		rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs", who, body)
		if rec.Code/100 != 2 && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("composing %s = %d (%s)", name, rec.Code, rec.Body)
		}
		return rec.Code
	}
	joined := func(job string) (scriptUID, scheduleUID string) {
		t.Helper()
		_ = pool.QueryRow(`SELECT COALESCE(script_uid, '') FROM jobs WHERE name = ? AND source = 'cronomicon'`, job).Scan(&scriptUID)
		_ = pool.QueryRow(`SELECT COALESCE(d.schedule_uid, '') FROM definition_schedules d JOIN jobs j ON j.uid = d.owner_uid WHERE j.name = ? AND j.source = 'cronomicon'`, job).Scan(&scheduleUID)
		return scriptUID, scheduleUID
	}
	for _, c := range []struct {
		what, who, job, scope, script, schedule string
		wantScript, wantSchedule                string
	}{
		{"FIN's job, a name every repository has", gFinAdmin, "fin-own", "fin-hosts", "deploy.sh", "nightly", "s-fin", "d-fin"},
		{"TAX's job, the same names", gTaxAdmin, "tax-own", "tax-hosts", "deploy.sh", "nightly", "s-tax", "d-tax"},
		{"FIN's job, names only Global's has", gFinAdmin, "fin-shared", "fin-hosts", "shared.sh", "weekly", "s-shared", "d-shared"},
		{"FIN's job, a schedule the app and Git both have", gFinAdmin, "fin-both", "fin-hosts", "deploy.sh", "both", "s-fin", "d-both-app"},
	} {
		if code := compose(c.who, c.job, c.scope, c.script, c.schedule); code/100 != 2 {
			t.Errorf("%s: composing = %d, want it saved", c.what, code)
			continue
		}
		if gotScript, gotSchedule := joined(c.job); gotScript != c.wantScript || gotSchedule != c.wantSchedule {
			t.Errorf("%s: joined to script %q and schedule %q; want %q and %q", c.what, gotScript, gotSchedule, c.wantScript, c.wantSchedule)
		}
	}
	// Names only ANOTHER agency's repository has do not resolve.
	if code := compose(gFinAdmin, "fin-wants-tax-script", "fin-hosts", "tax-only.sh", "weekly"); code != http.StatusUnprocessableEntity {
		t.Errorf("FIN's job naming a script only TAX's repository has = %d, want 422", code)
	}
	if code := compose(gFinAdmin, "fin-wants-tax-schedule", "fin-hosts", "shared.sh", "tax-only"); code != http.StatusUnprocessableEntity {
		t.Errorf("FIN's job naming a schedule only TAX's repository has = %d, want 422", code)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM jobs WHERE name LIKE 'fin-wants-%'`); n != 0 {
		t.Errorf("%d jobs were saved on another agency's script or schedule", n)
	}
}

// Where the composer looks a name up, for two cases the first tests left out
// (the review's mutations survived): a job on a scope SEVERAL agencies share
// is Global's for this purpose, not the first of them; and a workflow's
// schedule is taken from its owner's repository.
func TestTheComposersHomeForASharedScopeAndForAWorkflow(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'u', 'main'), ('repo-tax', 'ag:TAX', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc:both', 'both-hosts', 'cronomicon', '2026-01-01T00:00:00Z')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc:both'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc:both', 'ag:FIN'), ('sc:both', 'ag:TAX')`)
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES
	      ('s-fin', 'repo-fin', 'deploy.sh', 'bash', 'echo fin', 'h1', 't'), ('s-glob', 'global', 'deploy.sh', 'bash', 'echo glob', 'h2', 't')`)
	exec(`INSERT INTO schedules (uid, name, source, cron, content_hash, repo_id, owner_agency) VALUES
	      ('d-tax', 'nightly', 'git', '0 0 1 * * *', 'h', 'repo-tax', 'ag:TAX'),
	      ('d-glob', 'nightly', 'git', '0 0 2 * * *', 'h', 'global', 'global'),
	      ('d-fin', 'nightly', 'git', '0 0 3 * * *', 'h', 'repo-fin', 'ag:FIN')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled, created_at) VALUES
	      ('fin-job', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('tax-job', 'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z')`)

	// A job on the shared scope: its script is Global's.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs", gRoot, `{"name":"on-shared","scope":"both-hosts","scriptRef":"deploy.sh"}`)
	if rec.Code/100 != 2 {
		t.Fatalf("composing a job on the shared scope = %d (%s)", rec.Code, rec.Body)
	}
	var script string
	_ = pool.QueryRow(`SELECT COALESCE(script_uid, '') FROM jobs WHERE name = 'on-shared'`).Scan(&script)
	if script != "s-glob" {
		t.Errorf("a job on a scope two agencies share is joined to the script %q; want Global's", script)
	}

	// A workflow of FIN's jobs takes FIN's repository's schedule; of TAX's, TAX's.
	for who, c := range map[string][3]string{gFinAdmin: {"fin-flow", "fin-job", "d-fin"}, gTaxAdmin: {"tax-flow", "tax-job", "d-tax"}} {
		body := fmt.Sprintf(`{"name":%q,"steps":[{"type":"job","name":%q}],"scheduleRefs":["nightly"]}`, c[0], c[1])
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", who, body); rec.Code/100 != 2 {
			t.Fatalf("composing %s = %d (%s)", c[0], rec.Code, rec.Body)
		}
		var sched string
		_ = pool.QueryRow(`SELECT COALESCE(d.schedule_uid, '') FROM definition_schedules d JOIN workflows w ON w.uid = d.owner_uid WHERE w.name = ?`, c[0]).Scan(&sched)
		if sched != c[2] {
			t.Errorf("the workflow %s is bound to the schedule %q; want its owner's repository's, %q", c[0], sched, c[2])
		}
	}
}

// A workflow that runs a job whose scope is not in the catalog is Global's: the
// job's runs are Global's, and so is what the workflow reaches.
func TestAWorkflowOfAJobOnAnUnknownScopeIsGlobals(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled, created_at) VALUES
	      ('fin-job', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('odd-job', 'cronomicon', 'bash', 'no-such-scope', 1, '2026-01-01T00:00:00Z')`)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gRoot,
		`{"name":"odd-flow","steps":[{"type":"job","name":"fin-job"},{"type":"job","name":"odd-job"}]}`)
	if rec.Code/100 != 2 {
		t.Fatalf("composing = %d (%s)", rec.Code, rec.Body)
	}
	var owner string
	_ = pool.QueryRow(`SELECT owner_agency FROM workflows WHERE name = 'odd-flow'`).Scan(&owner)
	if owner != "global" {
		t.Errorf("a workflow of FIN's job and a job on a scope that is not there is %q's; want Global's", owner)
	}
}
