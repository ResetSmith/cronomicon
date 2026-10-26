package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 2.5.3 CA2: the built-in agency (Central; 'global') has no repository (CA-6).
// Its connection cannot be made, nothing syncs for it, and a connection it kept
// from before 2.5.3 is shown as a leftover and removed by a global
// administrator's repair, with what it synced (question 203).

func TestCA2CentralHasNoRepository(t *testing.T) {
	h, _ := gateServer(t)

	// Unconfigured (the row 1310 left): no repository.
	r := gateReq(t, h, http.MethodGet, "/api/v1/agencies/global/git-repo", gRoot, ``)
	if r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), "not_connected") {
		t.Errorf("Central's repository, unconfigured = %d %s, want 404 not_connected", r.Code, r.Body)
	}
	// It cannot be connected.
	r = gateReq(t, h, http.MethodPut, "/api/v1/agencies/global/git-repo", gRoot,
		`{"repoUrl":"https://git.example.com/central.git","pat":"t","writeBranch":"main"}`)
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "central_runs_nothing") {
		t.Errorf("connecting Central's repository = %d %s, want 422 central_runs_nothing", r.Code, r.Body)
	}
	// Nothing to sync, and the routes from before 2.4.0 that name no
	// repository say so rather than "try again".
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/git/sync"},
		{http.MethodPost, "/api/v1/git/sync"},
		{http.MethodPost, "/api/v1/scopes/resync"},
	} {
		r = gateReq(t, h, c.method, c.path, gRoot, `{}`)
		if r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), "not_connected") {
			t.Errorf("%s %s naming no repository = %d %s, want 404 not_connected", c.method, c.path, r.Code, r.Body)
		}
	}
	// Not listed among the repositories.
	r = gateReq(t, h, http.MethodGet, "/api/v1/git-repos", gRoot, ``)
	if strings.Contains(r.Body.String(), `"id":"global"`) {
		t.Errorf("the repositories list Central's: %s", r.Body)
	}
	// Nothing to remove.
	r = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gRoot, `{"repair":true,"reason":"ca2"}`)
	if r.Code != http.StatusNotFound {
		t.Errorf("removing Central's unconfigured row = %d %s, want 404", r.Code, r.Body)
	}
}

// A connection kept from before 2.5.3 (question 203): shown as a leftover, and
// removed only as a global administrator's repair with a reason, taking what
// it synced with it; the run history stays.
func TestCA2CentralsLeftoverRepositoryIsRemovedByARepair(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`UPDATE git_repos SET url = 'https://git.example.com/old-global.git' WHERE id = 'global'`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, repo_id, created_at) VALUES ('j-left', 'left-job', 'git', 'bash', 'global', 't')`)
	exec(`INSERT INTO scripts (repo_id, name, run_type, content_hash, synced_at) VALUES ('global', 'left.sh', 'bash', 'h', 't')`)
	exec(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES ('r-left', 'left-job', 'j-left', 'bash', 'success', 'x', 'manual', 't', '["Global"]')`)

	r := gateReq(t, h, http.MethodGet, "/api/v1/agencies/global/git-repo", gRoot, ``)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"leftover":true`) {
		t.Fatalf("Central's leftover connection = %d %s, want 200 marked leftover", r.Code, r.Body)
	}
	// An agency's administrator may not remove it; a global administrator only
	// as a repair with a reason.
	if r = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gFinAdmin, `{"repair":true,"reason":"x"}`); r.Code != http.StatusForbidden {
		t.Errorf("FIN's administrator removing it = %d %s, want 403", r.Code, r.Body)
	}
	if r = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gRoot, `{}`); r.Code < 400 {
		t.Errorf("removing it with no repair = %d %s, want a refusal", r.Code, r.Body)
	}
	r = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gRoot, `{"repair":true,"reason":"moved to Templates"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("removing it as a repair = %d %s, want 200", r.Code, r.Body)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM git_repos WHERE id = 'global'`); n != 0 {
		t.Errorf("the connection is still there (%d)", n)
	}
	if n := count(`SELECT COUNT(*) FROM jobs WHERE uid = 'j-left'`) + count(`SELECT COUNT(*) FROM scripts WHERE repo_id = 'global'`); n != 0 {
		t.Errorf("%d rows it synced are still there", n)
	}
	if n := count(`SELECT COUNT(*) FROM runs WHERE id = 'r-left'`); n != 1 {
		t.Errorf("its run history went with it (%d)", n)
	}
	if n := count(`SELECT COUNT(*) FROM change_log WHERE details LIKE '%moved to Templates%'`); n == 0 {
		t.Errorf("the repair's reason is not in the change log")
	}
	if r = gateReq(t, h, http.MethodGet, "/api/v1/agencies/global/git-repo", gRoot, ``); r.Code != http.StatusNotFound {
		t.Errorf("after the removal = %d %s, want 404", r.Code, r.Body)
	}
}

// An agency's repository is changed, never disconnected (IN-2): the removal
// is the built-in agency's leftover's only.
func TestCA2AnAgencysRepositoryIsNotRemoved(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	mustExec(t, pool)(`INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'https://git.example.com/fin.git', 'main')`) // the fixture may have given FIN one
	r := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/ag:FIN/git-repo", gRoot, `{"repair":true,"reason":"x"}`)
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "repository_not_removable") {
		t.Errorf("removing FIN's repository = %d %s, want 422 repository_not_removable", r.Code, r.Body)
	}
}

// A publish that names no repository meant Global's; the built-in agency has
// none since 2.5.3 (CA-6), so it is refused as for an agency that has not
// connected one, rather than "try again in a moment" (the review of CA2).
func TestCA2APublishNamingNoRepositoryIsRefusedAsNoRepository(t *testing.T) {
	h, _ := gateServer(t)
	r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/schedules/publish", gRoot,
		`{"filePath":"schedules/ca2.yaml","content":"apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: ca2\nspec:\n  cron: \"0 2 * * *\"\n","commitMessage":"ca2"}`,
		"If-Match", "0000000000000000000000000000000000000000")
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "no_repository") {
		t.Errorf("publishing with no repository named = %d %s, want 422 no_repository", r.Code, r.Body)
	}
}

// A Global repository configured through CRONOMICON_GITLAB_BASE_URL never had
// its URL in the row: its leftover is the rows it synced (the review of CA2).
// Those are recognised, and removed, as a connection with a URL is.
func TestCA2ALeftoverWithNoURLInItsRowIsStillALeftover(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('global', 'global', '', 'main')`)
	exec(`UPDATE git_repos SET url = '' WHERE id = 'global'`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, repo_id, created_at) VALUES ('j-env', 'from-env', 'git', 'bash', 'global', 't')`)

	r := gateReq(t, h, http.MethodGet, "/api/v1/agencies/global/git-repo", gRoot, ``)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"leftover":true`) {
		t.Fatalf("Central's repository from the environment, with what it synced = %d %s, want 200 marked leftover", r.Code, r.Body)
	}
	r = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gRoot, `{"repair":true,"reason":"from the environment"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("removing it = %d %s, want 200", r.Code, r.Body)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE uid = 'j-env'`).Scan(&n)
	if n != 0 {
		t.Errorf("the job it synced is still there")
	}
}

// A definition in the leftover is not assigned from: its repository no
// longer syncs, which answered "try again in a moment" for ever (the review of
// CA2). The way out is the leftover's removal and the URL reconnected to an
// agency.
func TestCA2ADefinitionInTheLeftoverIsNotAssignedFrom(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`UPDATE git_repos SET url = 'https://git.example.com/old-global.git', last_sha = 'abc123' WHERE id = 'global'`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, repo_id, scope, source_path, created_at) VALUES ('j-left', 'left-job', 'git', 'bash', 'global', 'fin-hosts', 'jobs/left-job.yaml', 't')`)
	r := gateReq(t, h, http.MethodPost, "/api/v1/agencies/ag:FIN/assign", gRoot, `{"kind":"job","uid":"j-left","scope":"fin-hosts"}`)
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "central_runs_nothing") {
		t.Errorf("assigning a job of the leftover = %d %s, want 422 central_runs_nothing", r.Code, r.Body)
	}
}

// The removal is refused, and changes nothing, while one of the leftover's
// scopes is bound to a runner and has work queued: the rule an agency's
// deletion keeps (busyScopes).
func TestCA2ALeftoverIsNotRemovedWhileItsScopeHasWorkQueued(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`UPDATE git_repos SET url = 'https://git.example.com/old-global.git' WHERE id = 'global'`)
	exec(`INSERT INTO scopes (id, name, source, repo_id, created_at) VALUES ('sc-left', 'left-hosts', 'git', 'global', 't')`)
	exec(`INSERT INTO runners (id, name, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
	      VALUES ('rn-left', 'rn-left', 'offline', 'Linux', '["bash"]', 0, 1, '1.0', 't', 't', 'global')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-left', 'rn-left', 'rn-left', 't')`)
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, scope, executor, agencies_json)
	      VALUES ('r-busy', 'x', 'bash', 'queued', 'u', 'manual', 't', 'left-hosts', 'runner', '["Global"]')`)

	r := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gRoot, `{"repair":true,"reason":"x"}`)
	if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "scopes_busy") || !strings.Contains(r.Body.String(), "left-hosts") {
		t.Fatalf("removing it while its scope has work queued = %d %s, want 409 scopes_busy naming the scope", r.Code, r.Body)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM git_repos WHERE id = 'global'`).Scan(&n)
	if n != 1 {
		t.Errorf("the refused removal took the connection")
	}
	exec(`UPDATE runs SET status = 'killed' WHERE id = 'r-busy'`)
	if r = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global/git-repo", gRoot, `{"repair":true,"reason":"x"}`); r.Code != http.StatusOK {
		t.Errorf("removing it once the work is gone = %d %s, want 200", r.Code, r.Body)
	}
}
