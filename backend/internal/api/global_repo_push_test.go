package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// From the review of I3: Global's connection is tested before it is written
// (PUT /agencies/global/git-repo; /settings/gitlab, which was tested the same,
// was removed in 2.5.0, question 60); a Global with no URL (and none in the
// environment) is refused nothing (question 39); and an edit that changes the
// token alone is tested again, with the token it names.
func TestI3_GlobalsConnectionIsTestedOnEitherRouteAndAGlobalWithNoneIsRefusedNothing(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`UPDATE git_repos SET url = '' WHERE id = 'global'`)
	put := func(path, body string) (int, string) {
		rec := gateReq(t, h, http.MethodPut, path, gRoot, body)
		return rec.Code, rec.Body.String()
	}

	// No URL, none in the environment: a save of its other settings is not
	// refused, on either route.
	for _, path := range []string{"/api/v1/agencies/global/git-repo"} {
		if code, body := put(path, `{"botName":"Global Bot","writeBranch":"main"}`); code != http.StatusOK {
			t.Errorf("saving Global's bot name with no repository on %s = %d %s, want 200", path, code, body)
		}
	}

	// A URL with a token that may only read: refused on either route.
	for _, path := range []string{"/api/v1/agencies/global/git-repo"} {
		if code, body := put(path, `{"repoUrl":"https://git.example.com/global.git","pat":"glpat-read-only","writeBranch":"main"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "token_cannot_push") {
			t.Errorf("saving Global's connection with a read-only token on %s = %d %s, want 422 token_cannot_push", path, code, body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = 'global' AND url <> ''`); n != 0 {
		t.Fatalf("a refused save stored Global's URL")
	}

	// One that may push; then an edit of the token alone, to one that may
	// only read, is tested again and refused.
	if code, body := put("/api/v1/agencies/global/git-repo", `{"repoUrl":"https://git.example.com/global.git","pat":"glpat-global-1234","writeBranch":"main"}`); code != http.StatusOK {
		t.Fatalf("saving Global's connection = %d %s, want 200", code, body)
	}
	if code, body := put("/api/v1/agencies/global/git-repo", `{"repoUrl":"https://git.example.com/global.git","pat":"glpat-read-only-2","writeBranch":"main"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "token_cannot_push") {
		t.Errorf("changing Global's token alone to one that may only read = %d %s, want 422 token_cannot_push", code, body)
	}
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
