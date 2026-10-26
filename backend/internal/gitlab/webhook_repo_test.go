package gitlab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// A webhook delivery is for the repository its route names (GR-20): it is
// checked against that repository's secret, gated by that repository's
// settings, and syncs that repository and no other.
//
// The route with no id is the one every installation's hook pointed at before
// 2.4.0, and means Global's. The built-in agency has no repository since 2.5.3
// (CA-6): a delivery to it, or to `global` by its id, is answered as one for a
// repository that is not there, Central's own secret on a connection it kept
// included, and syncs nothing. (Until 2.5.3 it synced Global's repository; and
// the first repository here was Global's, it is ag-a's.)
//
// Until Phase R3 there was the one route, one secret set and one Service.
func TestGR3_AWebhookDeliveryIsForItsRepository(t *testing.T) {
	first, _, remoteA := newSyncFixture(t)
	pool := first.db
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
	enc := func(plain string) string {
		t.Helper()
		tok, err := secrets.EncryptString(cfg, plain)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if _, err := pool.Exec(`UPDATE git_repos SET webhook_secret_enc = ? WHERE id = ?`, enc("secret-of-a"), fixtureRepo); err != nil {
		t.Fatal(err)
	}
	// Central's connection from before 2.5.3, with its secret.
	if _, err := pool.Exec(`UPDATE git_repos SET url = ?, branch = ?, webhook_secret_enc = ? WHERE id = 'global'`,
		remoteA, first.Cfg.GitLabWriteBranch, enc("secret-of-global")); err != nil {
		t.Fatal(err)
	}
	remoteB := t.TempDir()
	repoB, err := gogit.PlainInit(remoteB, false)
	if err != nil {
		t.Fatal(err)
	}
	grAgencyScope(t, pool, "ag-b", "hosts-of-repo-b")
	grConfinedTo.Store(remoteB, "hosts-of-repo-b")
	gitCommitFile(t, repoB, remoteB, "jobs/agency-job.yaml", jobYAML("agency-job"), "an agency's job")
	headB, _ := repoB.Head()
	if _, err := pool.Exec(`INSERT INTO git_repos (id, agency_id, url, branch, webhook_secret_enc) VALUES ('repo-b', 'ag-b', ?, ?, ?)`,
		remoteB, headB.Name().Short(), enc("secret-of-b")); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	clones := t.TempDir()
	reg.cloneDir = func(repoID string) string { return filepath.Join(clones, repoID) }
	t.Cleanup(reg.Close)
	if err := reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	settle := func() {
		t.Helper()
		for _, id := range []string{fixtureRepo, "repo-b"} {
			if svc := reg.Service(id); svc != nil {
				svc.wg.Wait()
			}
		}
	}
	eventually(t, "both repositories' first syncs", func() bool {
		return grString(t, pool, `SELECT COALESCE(last_sha,'') FROM git_repos WHERE id='repo-a'`) != "" &&
			grString(t, pool, `SELECT COALESCE(last_sha,'') FROM git_repos WHERE id='repo-b'`) != ""
	})
	settle()
	if reg.Service(repoid.Global) != nil {
		t.Fatal("the registry started a Service for Central's leftover connection")
	}

	h := NewRegistryHandlers(reg)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/gitlab", h.WebhookGitLab)
	mux.HandleFunc("POST /api/v1/webhooks/gitlab/{repoId}", h.WebhookGitLab)
	deliver := func(path, token string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("X-Gitlab-Token", token)
		req.Header.Set("X-Gitlab-Event", "Push Hook")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		settle()
		return rec.Code, rec.Body.String()
	}
	synced := func(repo string) int {
		t.Helper()
		return grCount(t, pool, `SELECT COUNT(*) FROM git_sync_events WHERE repo_id = ? AND triggered_by = 'webhook'`, repo)
	}
	expect := func(what string, a, b int) {
		t.Helper()
		if g, o := synced(fixtureRepo), synced("repo-b"); g != a || o != b {
			t.Errorf("%s: webhook syncs of ag-a's = %d and of ag-b's = %d, want %d and %d", what, g, o, a, b)
		}
		if n := grCount(t, pool, `SELECT COUNT(*) FROM git_sync_events WHERE repo_id = 'global'`); n != 0 {
			t.Errorf("%s: %d sync(s) of Central's repository", what, n)
		}
	}
	const hook = "/api/v1/webhooks/gitlab"

	// The agency's repository's hook, with its secret: it syncs, the other does not.
	if code, body := deliver(hook+"/repo-b", "secret-of-b"); code != http.StatusAccepted {
		t.Fatalf("a delivery to the agency's repository's hook = %d (%s), want 202", code, body)
	}
	expect("after a delivery to the agency's hook", 0, 1)
	if code, body := deliver(hook+"/repo-a", "secret-of-a"); code != http.StatusAccepted {
		t.Fatalf("a delivery to ag-a's repository's hook = %d (%s), want 202", code, body)
	}
	expect("after a delivery to ag-a's hook", 1, 1)

	// A secret opens its own repository's hook and no other.
	_, wrongToken := deliver(hook+"/repo-b", "not-a-secret")
	for path, token := range map[string]string{
		hook + "/repo-b": "secret-of-a",
		hook + "/repo-a": "secret-of-b",
	} {
		if code, body := deliver(path, token); code != http.StatusUnauthorized {
			t.Errorf("%s with another repository's secret = %d (%s), want 401", path, code, body)
		}
	}
	// A repository that does not exist answers as a wrong token does, to the
	// byte; and so does the built-in agency's, by the route with no id or by
	// its id, whatever the secret, Central's own included.
	for path, token := range map[string]string{
		hook + "/no-such-repository": "secret-of-a",
		hook:                         "secret-of-global",
		hook + "/global":             "secret-of-global",
	} {
		if code, body := deliver(path, token); code != http.StatusUnauthorized || body != wrongToken {
			t.Errorf("a delivery to %s = %d %q, want what a wrong token gets: 401 %q", path, code, body, wrongToken)
		}
	}
	if code, body := deliver(hook, "secret-of-a"); code != http.StatusUnauthorized || body != wrongToken {
		t.Errorf("a delivery to the route with no id with an agency's secret = %d %q, want 401 %q", code, body, wrongToken)
	}
	expect("after the refused deliveries", 1, 1)

	// The agency turns its webhook off: its hook is refused, the other's is not.
	if _, err := pool.Exec(`UPDATE git_repos SET webhook_enabled = 0 WHERE id = 'repo-b'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := deliver(hook+"/repo-b", "secret-of-b"); code != http.StatusForbidden {
		t.Errorf("a delivery to a repository whose webhook is off = %d, want 403", code)
	}
	if code, _ := deliver(hook+"/repo-a", "secret-of-a"); code != http.StatusAccepted {
		t.Errorf("a delivery to ag-a's hook while the agency's is off = %d, want 202", code)
	}
	expect("after the agency turned its webhook off", 2, 1)
}
