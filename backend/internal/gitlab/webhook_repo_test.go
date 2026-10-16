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
// settings, and syncs that repository and no other. The route with no id is
// the one every installation's hook already points at, and means Global's.
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
	if _, err := pool.Exec(`UPDATE git_repos SET url = ?, branch = ?, webhook_secret_enc = ? WHERE id = 'global'`,
		remoteA, first.Cfg.GitLabWriteBranch, enc("secret-of-global")); err != nil {
		t.Fatal(err)
	}
	remoteB := t.TempDir()
	repoB, err := gogit.PlainInit(remoteB, false)
	if err != nil {
		t.Fatal(err)
	}
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
		for _, id := range []string{repoid.Global, "repo-b"} {
			if svc := reg.Service(id); svc != nil {
				svc.wg.Wait()
			}
		}
	}
	eventually(t, "both repositories' first syncs", func() bool {
		return grString(t, pool, `SELECT COALESCE(last_sha,'') FROM git_repos WHERE id='global'`) != "" &&
			grString(t, pool, `SELECT COALESCE(last_sha,'') FROM git_repos WHERE id='repo-b'`) != ""
	})
	settle()

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
	expect := func(what string, global, b int) {
		t.Helper()
		if g, o := synced(repoid.Global), synced("repo-b"); g != global || o != b {
			t.Errorf("%s: webhook syncs of Global's = %d and of the agency's = %d, want %d and %d", what, g, o, global, b)
		}
	}
	const hook = "/api/v1/webhooks/gitlab"

	// The agency's repository's hook, with its secret: it syncs, Global's does not.
	if code, body := deliver(hook+"/repo-b", "secret-of-b"); code != http.StatusAccepted {
		t.Fatalf("a delivery to the agency's repository's hook = %d (%s), want 202", code, body)
	}
	expect("after a delivery to the agency's hook", 0, 1)

	// The route with no id, with Global's secret: Global's syncs, the agency's does not.
	if code, body := deliver(hook, "secret-of-global"); code != http.StatusAccepted {
		t.Fatalf("a delivery to the old hook URL = %d (%s), want 202", code, body)
	}
	expect("after a delivery to the old hook URL", 1, 1)
	// `global` in the path is the same repository.
	if code, body := deliver(hook+"/global", "secret-of-global"); code != http.StatusAccepted {
		t.Fatalf("a delivery to /global = %d (%s), want 202", code, body)
	}
	expect("after a delivery to Global's route by its id", 2, 1)

	// A secret opens its own repository's hook and no other.
	_, wrongToken := deliver(hook+"/repo-b", "not-a-secret")
	for path, token := range map[string]string{
		hook + "/repo-b": "secret-of-global",
		hook:             "secret-of-b",
		hook + "/global": "secret-of-b",
	} {
		if code, body := deliver(path, token); code != http.StatusUnauthorized {
			t.Errorf("%s with another repository's secret = %d (%s), want 401", path, code, body)
		}
	}
	// A repository that does not exist answers as a wrong token does, to the byte.
	if code, body := deliver(hook+"/no-such-repository", "secret-of-global"); code != http.StatusUnauthorized || body != wrongToken {
		t.Errorf("a delivery for a repository that does not exist = %d %q, want what a wrong token gets: 401 %q", code, body, wrongToken)
	}
	expect("after the refused deliveries", 2, 1)

	// The agency turns its webhook off: its hook is refused, Global's is not.
	if _, err := pool.Exec(`UPDATE git_repos SET webhook_enabled = 0 WHERE id = 'repo-b'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := deliver(hook+"/repo-b", "secret-of-b"); code != http.StatusForbidden {
		t.Errorf("a delivery to a repository whose webhook is off = %d, want 403", code)
	}
	if code, _ := deliver(hook, "secret-of-global"); code != http.StatusAccepted {
		t.Errorf("a delivery to Global's hook while the agency's is off = %d, want 202", code)
	}
	expect("after the agency turned its webhook off", 3, 1)
}
