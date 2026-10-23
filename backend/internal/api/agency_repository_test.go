package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An agency is created together with its repository, and deleted, when it is
// empty, with it (§0a, IN-2, IN-13; Phase I3; the owner's answers to
// questions 37 and 38).
func TestI3_AnAgencyIsCreatedWithItsRepositoryAndDeletedWithIt(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES ('gitAllowedHosts', '["git.example.com"]', 't', 't')`)
	agencies := func() int { return count(t, pool, `SELECT COUNT(*) FROM agencies WHERE name = 'Ops'`) }
	create := func(body string) (int, string) {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/agencies", gRoot, body)
		return rec.Code, rec.Body.String()
	}

	// Not with a repository given without its URL (since 2.5.1, SG-40, an
	// agency may be created with none: agency_without_repository_test.go),
	// nor with one whose token may only read, nor with one that does not
	// answer; and nothing is written.
	for _, c := range []struct{ body, code string }{
		{`{"name":"Ops","repository":{"pat":"glpat-ops"}}`, "validation_failed"},
		{`{"name":"Ops","repository":{"repoUrl":"https://git.example.com/ops/read-only.git","pat":"glpat-ops"}}`, "token_cannot_push"},
		{`{"name":"Ops","repository":{"repoUrl":"https://git.example.com/ops/unreachable.git","pat":"glpat-ops"}}`, "connection_failed"},
		{`{"name":"Ops","repository":{"repoUrl":"https://elsewhere.example.net/ops.git","pat":"glpat-ops"}}`, "repo_url_not_allowed"},
	} {
		if code, body := create(c.body); code != http.StatusUnprocessableEntity || !strings.Contains(body, c.code) {
			t.Errorf("creating %s = %d %s, want 422 %s", c.body, code, body, c.code)
		}
	}
	if n := agencies(); n != 0 {
		t.Fatalf("a refused create left %d agency rows", n)
	}

	// With one that may push: the agency and its repository, in one answer.
	code, body := create(`{"name":"Ops","repository":{"repoUrl":"https://git.example.com/ops/defs.git","pat":"glpat-ops-1234"}}`)
	var made struct {
		ID         string
		Repository struct {
			ID, AgencyID, RepoUrl string
			PatSet                bool
		}
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &made) != nil || made.ID == "" ||
		made.Repository.AgencyID != made.ID || made.Repository.RepoUrl != "https://git.example.com/ops/defs.git" || !made.Repository.PatSet {
		t.Fatalf("creating Ops with its repository = %d %s", code, body)
	}
	if strings.Contains(body, "glpat-ops-1234") {
		t.Errorf("the answer carries the token: %s", body)
	}
	repoID := made.Repository.ID

	// What its repository supplied, and what it built in the app.
	exec(`INSERT INTO scopes (id, name, source, repo_id, created_at, owner_agency) VALUES ('sc-ops', 'ops-hosts', 'git', ?, 't', ?)`, repoID, made.ID)
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc-ops' AND agency_id = ?`, made.ID); n != 1 {
		t.Fatalf("the Git scope is not Ops's member (%d)", n)
	}
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, repo_id, synced_at) VALUES ('j-ops-git', 'ops-git', 'git', 'bash', 'ops-hosts', ?, 't')`, repoID)
	// Its scope is bound to a runner, and has work queued.
	exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
	      VALUES ('agent-g', 'agent-g', 'agent', 'offline', 'Linux', '["bash"]', 0, 5, '1.0', 't', 't')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-ops', 'agent-g', 'agent-g', 't')`)
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, executor, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES ('r-wait', 'ops-git', 'bash', 'ops-hosts', 'queued', 'runner', 'u', 'manual', 't', '["Ops"]')`)
	exec(`INSERT INTO calendars (source, name, created_at, owner_agency) VALUES ('git', 'ops-holidays', 't', ?)`, made.ID)
	exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, owner_agency, deleted_at) VALUES ('j-ops-app', 'ops-app', 'cronomicon', 'bash', 't', ?, '2026-01-01T00:00:00Z')`, made.ID)

	// A job it built in the app — in the recycle bin, even — keeps it.
	rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/"+made.ID, gRoot, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "built in the app") {
		t.Fatalf("deleting Ops while it has a job built in the app = %d %s, want 409 naming it", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = ?`, repoID); n != 1 {
		t.Fatalf("a refused delete took the repository")
	}

	// Without it, the agency is empty: what its repository supplied does not
	// keep it (it is all still in Git), and goes with the connection.
	exec(`DELETE FROM jobs WHERE uid = 'j-ops-app'`)
	// Work queued on its repository's bound scope holds the delete (GR-28's
	// rule, which the disconnect it now uses applies), and changes nothing.
	rec = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/"+made.ID, gRoot, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "scopes_busy") || !strings.Contains(rec.Body.String(), "ops-hosts") {
		t.Fatalf("deleting Ops with work queued on its bound scope = %d %s, want 409 scopes_busy naming it", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = ?`, repoID) + count(t, pool, `SELECT COUNT(*) FROM agencies WHERE id = ?`, made.ID); n != 2 {
		t.Fatalf("a delete held by queued work changed something")
	}
	exec(`DELETE FROM runs WHERE id = 'r-wait'`)
	rec = gateReq(t, h, http.MethodDelete, "/api/v1/agencies/"+made.ID, gRoot, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deleting the empty Ops = %d %s, want 204", rec.Code, rec.Body)
	}
	for what, q := range map[string]string{
		"the agency":         `SELECT COUNT(*) FROM agencies WHERE id = ?`,
		"its repository":     `SELECT COUNT(*) FROM git_repos WHERE agency_id = ?`,
		"its Git scope":      `SELECT COUNT(*) FROM scopes WHERE owner_agency = ?`,
		"its Git job":        `SELECT COUNT(*) FROM jobs WHERE owner_agency = ?`,
		"its Git calendar":   `SELECT COUNT(*) FROM calendars WHERE owner_agency = ?`,
		"its scope's member": `SELECT COUNT(*) FROM scope_agencies WHERE agency_id = ?`,
	} {
		if n := count(t, pool, q, made.ID); n != 0 {
			t.Errorf("after the delete, %s is still there (%d)", what, n)
		}
	}

	// An agency's repository is changed, not disconnected. The route came back
	// in 2.5.3 for the built-in agency's leftover connection alone (CA-6); for
	// an agency's it refuses, as the route's absence did.
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/ag:FIN/git-repo", gRoot, ""); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "repository_not_removable") {
		t.Errorf("DELETE …/git-repo of an agency's = %d %s, want 422 repository_not_removable", rec.Code, rec.Body)
	}
}
