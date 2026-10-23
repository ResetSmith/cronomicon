package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// From the review of I3: Global's connection was tested before it was written
// (PUT /agencies/global/git-repo; /settings/gitlab, which was tested the same,
// was removed in 2.5.0, question 60), a Global with no URL (and none in the
// environment) was refused nothing (question 39), and an edit that changes the
// token alone is tested again, with the token it names.
//
// Since 2.5.3 the built-in agency has no repository (CA-6): every write of its
// connection is refused, 422 central_runs_nothing, whatever it carries (a bot
// name alone, a token that may only read, one that may push), and nothing is
// stored. The token-alone edit is an agency's repository's now.
func TestI3_GlobalsConnectionIsRefusedAndAnEditOfTheTokenAloneIsTestedAgain(t *testing.T) {
	h, pool := repoGateServer(t)
	exec := mustExec(t, pool)
	exec(`UPDATE git_repos SET url = '', bot_name = 'cronomicon-bot' WHERE id = 'global'`)
	put := func(path, body string) (int, string) {
		rec := gateReq(t, h, http.MethodPut, path, gRoot, body)
		return rec.Code, rec.Body.String()
	}

	for _, body := range []string{
		`{"botName":"Global Bot","writeBranch":"main"}`,
		`{"repoUrl":"https://git.example.com/global.git","pat":"glpat-read-only","writeBranch":"main"}`,
		`{"repoUrl":"https://git.example.com/global.git","pat":"glpat-global-1234","writeBranch":"main"}`,
	} {
		if code, got := put("/api/v1/agencies/global/git-repo", body); code != http.StatusUnprocessableEntity || !strings.Contains(got, "central_runs_nothing") {
			t.Errorf("saving Global's connection %s = %d %s, want 422 central_runs_nothing", body, code, got)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = 'global' AND (url <> '' OR bot_name <> 'cronomicon-bot' OR token_enc IS NOT NULL)`); n != 0 {
		t.Fatalf("a refused save of Global's connection stored something")
	}

	// An agency's: one that may push; then an edit of the token alone, to one
	// that may only read, is tested again and refused.
	const finPath = "/api/v1/agencies/ag:FIN/git-repo"
	if code, body := put(finPath, `{"repoUrl":"http://127.0.0.1:1/fin.git","pat":"glpat-fin-1234","writeBranch":"main"}`); code != http.StatusCreated {
		t.Fatalf("connecting FIN's repository = %d %s, want 201", code, body)
	}
	if code, body := put(finPath, `{"repoUrl":"http://127.0.0.1:1/fin.git","pat":"glpat-read-only-2","writeBranch":"main"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "token_cannot_push") {
		t.Errorf("changing FIN's token alone to one that may only read = %d %s, want 422 token_cannot_push", code, body)
	}
	syncsSettled(t, pool)
}

// 2.5.0 (question 60): /settings/gitlab and its webhook-secret rotation are
// gone; /agencies/global/git-repo is Global's repository's connection.
func TestTheSettingsGitlabRoutesAreGone(t *testing.T) {
	h, _ := gateServer(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/settings/gitlab"},
		{http.MethodPut, "/api/v1/settings/gitlab"},
		{http.MethodPost, "/api/v1/settings/gitlab/webhook-secret/rotate"},
	} {
		if rec := gateReq(t, h, c.method, c.path, gRoot, `{}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want it gone", c.method, c.path, rec.Code)
		}
	}
}
