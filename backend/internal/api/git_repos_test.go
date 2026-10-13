package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoGateServer is gateServer for the tests that connect a repository. A
// connection that is accepted is synced at once, in the background, so:
//
//   - the clones go under the test's own directory, not the machine's;
//   - the hosts allowed for an agency's repository are git.example.com, which
//     the tests name and never reach, and 127.0.0.1:1, where the connections
//     they make point: the outbound guard refuses loopback at the dial, so the
//     sync fails at once and nothing leaves the machine.
func repoGateServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(t.TempDir(), "git-cache", "job-definitions"))
	h, pool := gateServer(t)
	mustExec(t, pool)(`INSERT INTO settings (key, value, last_modified_by, last_modified_at)
	                   VALUES ('gitAllowedHosts', '["git.example.com","127.0.0.1:1"]', 't', 't')`)
	return h, pool
}

// syncsSettled waits until the background syncs have stopped writing: the
// count of sync events has not moved for a while.
func syncsSettled(t *testing.T, pool *sql.DB) {
	t.Helper()
	count := func() int {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM git_sync_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(30 * time.Second)
	last, since := count(), time.Now()
	for time.Since(since) < 500*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatalf("the background syncs did not settle")
		}
		time.Sleep(50 * time.Millisecond)
		if n := count(); n != last {
			last, since = n, time.Now()
		}
	}
}

// A repository per agency (2.4.0 Phase R6; GR-9, GR-10).
//
// An agency's administrator connects, reads and edits THEIR agency's
// repository, with the agency's own token, on a host a global administrator
// has listed. They cannot do it for another agency or for Global, and are told
// nothing about either. A global administrator can for any.
func TestAnAgencysAdministratorConnectsTheirAgencysRepository(t *testing.T) {
	h, pool := repoGateServer(t)

	type repo struct {
		ID, AgencyID, AgencyName, RepoUrl, Pat, WriteBranch, BotName string
		PatSet                                                       bool
		Supplies                                                     map[string]int
	}
	put := func(who, agency, body string) (int, repo, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/"+agency+"/git-repo", who, body)
		var out repo
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	get := func(who, agency string) (int, repo) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/agencies/"+agency+"/git-repo", who, "")
		var out repo
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	list := func(who string) (int, []string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/git-repos", who, "")
		var out struct {
			Items []struct{ ID, AgencyID string }
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var agencies []string
		for _, it := range out.Items {
			agencies = append(agencies, it.AgencyID)
		}
		return rec.Code, agencies
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM git_repos WHERE id <> 'global'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const conn = `{"repoUrl":"http://127.0.0.1:1/fin/definitions.git","pat":"glpat-fin-secret-1234","writeBranch":"main","webhookEnabled":true}`

	// Nothing is connected yet: an administrator of the agency is told so, and
	// nobody else is told anything.
	if code, _ := get(gFinAdmin, "ag:FIN"); code != http.StatusNotFound {
		t.Errorf("FIN's administrator reading FIN's repository before one is connected = %d, want 404", code)
	}
	if code, _ := get(gTaxAdmin, "ag:FIN"); code != http.StatusForbidden {
		t.Errorf("TAX's administrator reading FIN's repository = %d, want 403", code)
	}

	// Who may not connect FIN's repository.
	for _, who := range []string{gTaxAdmin, gFinApprover, gFinOperator, gFinViewer, gAllViewer} {
		if code, _, _ := put(who, "ag:FIN", conn); code != http.StatusForbidden {
			t.Errorf("%s connecting FIN's repository = %d, want 403", who, code)
		}
	}
	// FIN's administrator, for another agency, for Global, and for an agency
	// that is not there: one answer.
	for _, agency := range []string{"ag:TAX", "global", "ag:NOPE"} {
		if code, _, _ := put(gFinAdmin, agency, conn); code != http.StatusForbidden {
			t.Errorf("FIN's administrator connecting %s's repository = %d, want 403", agency, code)
		}
		if code, _ := get(gFinAdmin, agency); code != http.StatusForbidden {
			t.Errorf("FIN's administrator reading %s's repository = %d, want 403", agency, code)
		}
	}
	if n := rows(); n != 0 {
		t.Fatalf("%d repositories were connected by callers who may not", n)
	}

	// A host that is not listed, and a URL that is not one: refused, and
	// nothing is stored.
	for _, bad := range []string{
		`{"repoUrl":"https://evil.example.net/fin.git"}`,
		`{"repoUrl":"file:///var/lib/cronomicon/git-cache/job-definitions"}`,
		`{"repoUrl":"/var/lib/cronomicon/git-cache/job-definitions"}`,
		`{"repoUrl":"https://tok@git.example.com/fin.git"}`,
		`{"repoUrl":"http://127.0.0.1:2/fin.git"}`,
	} {
		code, _, body := put(gFinAdmin, "ag:FIN", bad)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "repo_url_not_allowed") {
			t.Errorf("connecting %s = %d %s, want 422 repo_url_not_allowed", bad, code, body)
		}
	}
	if code, _, body := put(gFinAdmin, "ag:FIN", `{"pat":"x"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "repoUrl is required") {
		t.Errorf("connecting with no URL = %d %s, want 422", code, body)
	}
	if n := rows(); n != 0 {
		t.Fatalf("a refused connection stored %d rows", n)
	}

	// FIN's administrator connects FIN's.
	code, made, body := put(gFinAdmin, "ag:FIN", conn)
	if code != http.StatusCreated {
		t.Fatalf("FIN's administrator connecting FIN's repository = %d %s, want 201", code, body)
	}
	if made.AgencyID != "ag:FIN" || made.AgencyName != "FIN" || !strings.HasPrefix(made.ID, "repo-") ||
		made.RepoUrl != "http://127.0.0.1:1/fin/definitions.git" || made.BotName != "cronomicon-bot" {
		t.Errorf("the connected repository reads %+v", made)
	}
	if !made.PatSet || strings.Contains(body, "glpat-fin-secret") {
		t.Errorf("the token: patSet %v, and the answer is %s; want it set and masked", made.PatSet, body)
	}
	var stored string
	if err := pool.QueryRow(`SELECT COALESCE(token_enc, '') FROM git_repos WHERE id = ?`, made.ID).Scan(&stored); err != nil ||
		stored == "" || strings.Contains(stored, "glpat-fin-secret") {
		t.Errorf("the stored token is %q (%v), want it there and encrypted", stored, err)
	}

	// It is read back by the agency's administrator and by a global one, and
	// by nobody else.
	if code, got := get(gFinAdmin, "ag:FIN"); code != http.StatusOK || got.ID != made.ID {
		t.Errorf("FIN's administrator reading it = %d %+v", code, got)
	}
	if code, got := get(gRoot, "ag:FIN"); code != http.StatusOK || got.ID != made.ID {
		t.Errorf("a global administrator reading it = %d %+v", code, got)
	}
	for _, who := range []string{gTaxAdmin, gFinApprover, gAllViewer} {
		if code, _ := get(who, "ag:FIN"); code != http.StatusForbidden {
			t.Errorf("%s reading FIN's connected repository = %d, want 403", who, code)
		}
	}
	// gMixed reaches every agency as a viewer and administers FIN: FIN's is
	// theirs to read, TAX's and Global's are not.
	if code, _ := get(gMixed, "ag:FIN"); code != http.StatusOK {
		t.Errorf("an administrator of FIN who also views everything, reading FIN's = %d, want 200", code)
	}
	if code, _ := get(gMixed, "global"); code != http.StatusForbidden {
		t.Errorf("the same caller reading Global's = %d, want 403", code)
	}

	// The list: each sees what they may configure.
	if code, got := list(gFinAdmin); code != http.StatusOK || strings.Join(got, ",") != "ag:FIN" {
		t.Errorf("FIN's administrator's list = %d %v, want FIN's alone", code, got)
	}
	if code, got := list(gTaxAdmin); code != http.StatusOK || len(got) != 0 {
		t.Errorf("TAX's administrator's list = %d %v, want none", code, got)
	}
	if code, got := list(gRoot); code != http.StatusOK || strings.Join(got, ",") != "global,ag:FIN" {
		t.Errorf("a global administrator's list = %d %v, want Global's and FIN's", code, got)
	}
	if code, _ := list(gFinViewer); code != http.StatusForbidden {
		t.Errorf("a viewer's list = %d, want 403", code)
	}

	// An edit is an edit of the same repository (200), and a mask sent back
	// keeps the token.
	edit := `{"repoUrl":"http://127.0.0.1:1/fin/moved.git","pat":"` + made.Pat + `","writeBranch":"release","botName":"Finance Bot","botEmail":"fin@example.com"}`
	code, edited, body := put(gFinAdmin, "ag:FIN", edit)
	if code != http.StatusOK || edited.ID != made.ID || edited.RepoUrl != "http://127.0.0.1:1/fin/moved.git" ||
		edited.WriteBranch != "release" || edited.BotName != "Finance Bot" {
		t.Fatalf("the edit = %d %s", code, body)
	}
	var after string
	if err := pool.QueryRow(`SELECT COALESCE(token_enc, '') FROM git_repos WHERE id = ?`, made.ID).Scan(&after); err != nil || after != stored {
		t.Errorf("the token after an edit that sent the mask back is %q (%v), want it as it was", after, err)
	}
	if n := rows(); n != 1 {
		t.Errorf("after an edit there are %d repositories, want the one", n)
	}
	// An edit to a host that is not listed is refused, and the connection is
	// as it was.
	if code, _, _ := put(gFinAdmin, "ag:FIN", `{"repoUrl":"https://evil.example.net/fin.git"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("an edit to an unlisted host = %d, want 422", code)
	}
	if _, got := get(gFinAdmin, "ag:FIN"); got.RepoUrl != "http://127.0.0.1:1/fin/moved.git" {
		t.Errorf("after a refused edit the URL is %q", got.RepoUrl)
	}

	// A global administrator may connect any agency's, and writes Global's
	// through the same route with any URL (Global's is exempt from the list).
	if code, tax, body := put(gRoot, "ag:TAX", `{"repoUrl":"http://127.0.0.1:1/tax.git"}`); code != http.StatusCreated || tax.AgencyID != "ag:TAX" {
		t.Errorf("a global administrator connecting TAX's = %d %s", code, body)
	}
	// (A port that is not on the list: the list does not govern Global's.)
	if code, g, body := put(gRoot, "global", `{"repoUrl":"http://127.0.0.1:2/global.git","writeBranch":"main"}`); code != http.StatusOK || g.ID != "global" {
		t.Errorf("a global administrator writing Global's through this route = %d %s", code, body)
	}
	if code, _, _ := put(gRoot, "ag:NOPE", conn); code != http.StatusNotFound {
		t.Errorf("a global administrator connecting an agency that is not there = %d, want 404", code)
	}
}

// Disconnecting (GR-28): by who may connect, with what was removed in the
// answer; refused for Global's, and while a scope of the repository is bound
// to runners with work queued. An agency whose repository is gone can connect
// another.
func TestAnAgencysAdministratorDisconnectsTheirAgencysRepository(t *testing.T) {
	h, pool := repoGateServer(t)
	exec := mustExec(t, pool)
	const conn = `{"repoUrl":"http://127.0.0.1:1/fin/definitions.git"}`
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, conn)
	var made struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	// What the repository's sync would have written, and a scope of it that is
	// bound to a runner.
	exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, scope, source_path, synced_at) VALUES ('j-fin', 'fin-git-job', 'git', ?, 'bash', 'fin-hosts', 'jobs/fin.yaml', 't')`, made.ID)
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, content_hash, synced_at) VALUES ('s-fin', ?, 'fin.sh', 'bash', 'scripts/fin.sh', 'h', 't')`, made.ID)
	exec(`INSERT INTO scopes (id, name, source, repo_id, created_at, synced_at) VALUES ('sc:fin-git', 'fin-git-hosts', 'git', ?, 't', 't')`, made.ID)
	exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
	      VALUES ('agent-fin', 'agent-fin', 'agent', 'offline', 'Linux', '["bash"]', 0, 5, '1.0', 't', 't')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc:fin-git', 'agent-fin', 'agent-fin', 't')`)
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, executor, triggered_by, trigger_kind, created_at)
	      VALUES ('r-wait', 'fin-git-job', 'bash', 'fin-git-hosts', 'queued', 'runner', 'u', 'manual', 't')`)
	del := func(who, agency string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/"+agency+"/git-repo", who, "")
		return rec.Code, rec.Body.String()
	}
	connected := func() int {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM git_repos WHERE id = ?`, made.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The read says what a disconnect would remove.
	rec = gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, "")
	if !strings.Contains(rec.Body.String(), `"supplies":{"jobs":1,"workflows":0,"scripts":1,"schedules":0,"scopes":1}`) {
		t.Errorf("the repository's read does not say what it supplies: %s", rec.Body)
	}

	for _, who := range []string{gTaxAdmin, gFinApprover, gFinViewer, gAllViewer} {
		if code, _ := del(who, "ag:FIN"); code != http.StatusForbidden {
			t.Errorf("%s disconnecting FIN's repository = %d, want 403", who, code)
		}
	}
	if code, _ := del(gFinAdmin, "ag:TAX"); code != http.StatusForbidden {
		t.Errorf("FIN's administrator disconnecting TAX's = %d, want 403", code)
	}
	if code, _ := del(gFinAdmin, "global"); code != http.StatusForbidden {
		t.Errorf("FIN's administrator disconnecting Global's = %d, want 403", code)
	}
	if code, body := del(gRoot, "global"); code != http.StatusConflict || !strings.Contains(body, "global_repo") {
		t.Errorf("a global administrator disconnecting Global's = %d %s, want 409 global_repo", code, body)
	}
	if code, body := del(gRoot, "ag:TAX"); code != http.StatusNotFound || !strings.Contains(body, "not_connected") {
		t.Errorf("disconnecting an agency that has none = %d %s, want 404 not_connected", code, body)
	}

	// Work is queued on its bound scope.
	if code, body := del(gFinAdmin, "ag:FIN"); code != http.StatusConflict || !strings.Contains(body, "scopes_busy") || !strings.Contains(body, "fin-git-hosts") {
		t.Fatalf("disconnecting with work queued on a bound scope = %d %s, want 409 scopes_busy naming it", code, body)
	}
	if connected() != 1 {
		t.Fatalf("the refused disconnect removed the connection")
	}
	exec(`DELETE FROM runs WHERE id = 'r-wait'`)

	code, body := del(gFinAdmin, "ag:FIN")
	if code != http.StatusOK || !strings.Contains(body, `"removed":{"jobs":1,"workflows":0,"scripts":1,"schedules":0,"scopes":1}`) {
		t.Fatalf("FIN's administrator disconnecting FIN's = %d %s, want 200 with what was removed", code, body)
	}
	var left int
	if err := pool.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs WHERE repo_id = ?1) + (SELECT COUNT(*) FROM scripts WHERE repo_id = ?1) +
	                                (SELECT COUNT(*) FROM scopes WHERE repo_id = ?1) + (SELECT COUNT(*) FROM git_repos WHERE id = ?1)`, made.ID).Scan(&left); err != nil || left != 0 {
		t.Errorf("after the disconnect %d rows of the repository are left (%v)", left, err)
	}
	if code, _ := del(gFinAdmin, "ag:FIN"); code != http.StatusNotFound {
		t.Errorf("disconnecting it again = %d, want 404", code)
	}
	// The agency can connect another.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, conn); rec.Code != http.StatusCreated {
		t.Errorf("connecting again after a disconnect = %d %s, want 201", rec.Code, rec.Body)
	}
}
