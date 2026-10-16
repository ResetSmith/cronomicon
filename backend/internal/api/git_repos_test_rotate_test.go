package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Testing a connection, and rotating an agency's repository's webhook secret
// (2.4.0 Phase R6; GR-9, GR-10, GR-20). Both are the agency's administrator's,
// for their own agency's repository. A test stores nothing and cannot be aimed
// where a connection could not be saved. A rotated secret is that repository's
// alone, and is what its webhook route then takes.
func TestAnAgencysAdministratorTestsAndRotatesTheirRepositorysConnection(t *testing.T) {
	h, pool := repoGateServer(t)
	exec := mustExec(t, pool)
	post := func(who, path, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, path, who, body)
		return rec.Code, rec.Body.String()
	}
	rows := func() int {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM git_repos WHERE id <> 'global'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const test, rotate = "/api/v1/agencies/ag:FIN/git-repo/test", "/api/v1/agencies/ag:FIN/git-repo/webhook-secret/rotate"

	// ── Testing, before anything is connected ───────────────────────────────
	for _, who := range []string{gTaxAdmin, gFinApprover, gFinViewer, gAllViewer} {
		if code, _ := post(who, test, `{"repoUrl":"http://127.0.0.1:1/fin.git"}`); code != http.StatusForbidden {
			t.Errorf("%s testing FIN's connection = %d, want 403", who, code)
		}
		if code, _ := post(who, rotate, `{"updateGitlab":false}`); code != http.StatusForbidden {
			t.Errorf("%s rotating FIN's webhook secret = %d, want 403", who, code)
		}
	}
	for _, bad := range []string{
		`{"repoUrl":"https://evil.example.net/fin.git"}`,
		`{"repoUrl":"file:///var/lib/cronomicon/git-cache/job-definitions"}`,
		`{"repoUrl":"/etc"}`,
	} {
		if code, body := post(gFinAdmin, test, bad); code != http.StatusUnprocessableEntity || !strings.Contains(body, "repo_url_not_allowed") {
			t.Errorf("testing %s = %d %s, want 422 repo_url_not_allowed", bad, code, body)
		}
	}
	if code, body := post(gFinAdmin, test, `{}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "repoUrl is required") {
		t.Errorf("testing with no URL and nothing connected = %d %s, want 422", code, body)
	}
	// An allowed host, where nothing answers: the test is made, and says so.
	code, body := post(gFinAdmin, test, `{"repoUrl":"http://127.0.0.1:1/unreachable.git","pat":"glpat-test-token-9999","writeBranch":"main"}`)
	var res struct {
		OK          bool
		BranchFound bool
		Branch      string
		Error       string
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil || res.OK || res.Error == "" || res.Branch != "main" {
		t.Errorf("testing a listed host where nothing answers = %d %s, want 200 with ok false and the error", code, body)
	}
	if strings.Contains(body, "glpat-test-token") {
		t.Errorf("the test's answer carries the token: %s", body)
	}
	if rows() != 0 {
		t.Fatalf("a test stored a connection")
	}
	if code, _ := post(gFinAdmin, rotate, `{"updateGitlab":false}`); code != http.StatusNotFound {
		t.Errorf("rotating the secret of an agency with no repository = %d, want 404", code)
	}

	// ── Connected ───────────────────────────────────────────────────────────
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, `{"repoUrl":"http://127.0.0.1:1/fin.git","pat":"glpat-fin-1234","webhookEnabled":true,"webhookEvents":{"push":true,"mr":true,"tag":true}}`)
	var made struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	// A test with nothing in it is of the stored connection (which, here,
	// answers and may push).
	if code, body := post(gFinAdmin, test, ``); code != http.StatusOK || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"canPush":true`) {
		t.Errorf("testing the stored connection = %d %s, want 200 with ok and canPush", code, body)
	}
	// And a test of another URL, from the form, is still held to the list.
	if code, _ := post(gFinAdmin, test, `{"repoUrl":"https://evil.example.net/fin.git"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("testing an unlisted host from a connected repository's form = %d, want 422", code)
	}

	// ── Rotating ────────────────────────────────────────────────────────────
	secretOf := func(repo string) string {
		var s string
		if err := pool.QueryRow(`SELECT COALESCE(webhook_secret_enc, '') FROM git_repos WHERE id = ?`, repo).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	exec(`INSERT INTO git_repos (id, agency_id) VALUES ('global', 'global') ON CONFLICT(id) DO NOTHING`)
	globalBefore := secretOf("global")
	code, body = post(gFinAdmin, rotate, `{"updateGitlab":false}`)
	var rotated struct{ Secret string }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &rotated) != nil || len(rotated.Secret) < 32 {
		t.Fatalf("FIN's administrator rotating FIN's webhook secret = %d %s", code, body)
	}
	if enc := secretOf(made.ID); enc == "" || strings.Contains(enc, rotated.Secret) {
		t.Errorf("the repository's stored secret is %q, want it there and encrypted", enc)
	}
	if secretOf("global") != globalBefore {
		t.Errorf("rotating FIN's secret changed Global's")
	}
	// The repository's webhook route takes the new secret, and no other.
	hook := func(repo, token string) int {
		return gateReqWithHeader(t, h, http.MethodPost, "/api/v1/webhooks/gitlab/"+repo, gFinViewer, `{}`, "X-Gitlab-Token", token).Code
	}
	if got := hook(made.ID, rotated.Secret); got != http.StatusAccepted {
		t.Errorf("FIN's repository's webhook with its new secret = %d, want 202", got)
	}
	if got := hook(made.ID, "not-the-secret"); got != http.StatusUnauthorized {
		t.Errorf("FIN's repository's webhook with another secret = %d, want 401", got)
	}
	if got := hook("global", rotated.Secret); got != http.StatusUnauthorized {
		t.Errorf("Global's webhook with FIN's secret = %d, want 401", got)
	}

	// Updating the hook means a request to the repository's host: held to the
	// list, like a sync. The host is taken off it.
	exec(`UPDATE settings SET value = '["git.example.com"]' WHERE key = 'gitAllowedHosts'`)
	before := secretOf(made.ID)
	if code, body := post(gFinAdmin, rotate, `{"updateGitlab":true}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "repo_url_not_allowed") {
		t.Errorf("rotating with the hook updated, on a host no longer listed = %d %s, want 422 repo_url_not_allowed", code, body)
	}
	if secretOf(made.ID) != before {
		t.Errorf("a refused rotation changed the stored secret")
	}
}
