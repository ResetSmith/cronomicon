package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttpclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/ResetSmith/cronomicon/internal/gitlab"
)

// grLocalRepo makes a repository named name under root with one job in it, on
// the scope named, and returns its branch and its head commit.
func grLocalRepo(t *testing.T, root, name, job, scope string) (branch, sha string) {
	t.Helper()
	path := filepath.Join(root, name)
	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	rel := "jobs/" + job + ".yaml"
	if err := os.MkdirAll(filepath.Join(path, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + job + "\nspec:\n  run_type: bash\n  scope: " + scope + "\n  command: echo hi\n"
	if err := os.WriteFile(filepath.Join(path, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("a job", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Name().Short(), head.Hash().String()
}

// grSmartHTTPRoot serves the repositories under a directory over smart HTTP,
// by git http-backend itself, and returns the directory and the server's URL.
// An agency's repository is reached over http(s) on an allowed host (GR-10),
// so it is not a local path, as Global's could be.
//
// go-git's http transport is the guarded one, which refuses loopback, and is
// installed once per process: the guard is installed first, as it would be,
// so that its one install is spent; a plain client then serves this test, and
// the guarded one is put back after (as the gitlab package's own tests do).
func grSmartHTTPRoot(t *testing.T) (root, base string) {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("no git: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	root = t.TempDir()
	srv := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)

	_ = gitlab.TestConnection(context.Background(), nil, "http://127.0.0.1:1/none.git", "", "main")
	prevHTTP := githttpclient.Protocols["http"]
	githttpclient.InstallProtocol("http", gogithttp.NewClient(http.DefaultClient))
	t.Cleanup(func() { githttpclient.InstallProtocol("http", prevHTTP) })
	return root, srv.URL
}

// Saving a repository's connection applies it: the repository it names is
// synced at once, with no restart of the server, and saving another URL syncs
// that one (GR-11 and GR-12, through the route an administrator uses).
//
// Until 2.4.0 the sync service resolved the URL and the token once, when the
// routes were mounted; the save logged "changes take effect on restart"; and
// after the restart the existing clone still fetched from the old URL. This
// was Global's connection until 2.5.3; the built-in agency has no repository
// since (CA-6), so it is an ordinary agency's.
func TestGR2_ASavedConnectionIsSyncedWithoutARestart(t *testing.T) {
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(t.TempDir(), "clone"))
	a := newGRSchedAPI(t)
	root, base := grSmartHTTPRoot(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	a.exec(`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES ('gitAllowedHosts', ?, 't', 't')`,
		`["`+u.Host+`"]`)
	const repoPath = "/api/v1/agencies/" + workAgency + "/git-repo"
	jobs := func(name string) int {
		return a.count(`SELECT COUNT(*) FROM jobs WHERE source = 'git' AND name = ?`, name)
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for: %s", what)
	}
	connection := func(url, branch string) map[string]any {
		return map[string]any{
			"repoUrl": url, "writeBranch": branch, "botName": "cronomicon-bot", "botEmail": "bot@example.com",
			"tokenExpiryNotifyDays": 7, "webhookEnabled": true,
			"webhookEvents": map[string]bool{"push": true, "mr": true, "tag": true},
		}
	}
	status := func() string {
		raw := a.must(http.MethodGet, "/api/v1/git/sync?repo="+workRepo, nil, http.StatusOK)
		var st struct {
			LastSHA string `json:"lastSHA"`
		}
		_ = json.Unmarshal(raw, &st)
		return st.LastSHA
	}

	branch1, sha1 := grLocalRepo(t, root, "first", "from-first", workScope)
	first := base + "/first"
	a.must(http.MethodPut, repoPath, connection(first, branch1), http.StatusOK)
	wait("the repository that was just connected to be synced", func() bool { return jobs("from-first") == 1 })
	wait("the sync status to name its commit", func() bool { return status() == sha1 })

	// The connection is changed to another repository.
	branch2, sha2 := grLocalRepo(t, root, "second", "from-second", workScope)
	second := base + "/second"
	a.must(http.MethodPut, repoPath, connection(second, branch2), http.StatusOK)
	wait("the newly named repository to be synced", func() bool { return jobs("from-second") == 1 })
	wait("the sync status to name the new repository's commit", func() bool { return status() == sha2 })

	// The manual sync route goes to the service of the connection as it is NOW.
	before := a.count(`SELECT COUNT(*) FROM git_sync_events`)
	a.must(http.MethodPost, "/api/v1/git/sync?repo="+workRepo, nil, http.StatusAccepted)
	wait("the manual sync to be recorded", func() bool {
		return a.count(`SELECT COUNT(*) FROM git_sync_events WHERE triggered_by = 'manual'`) >= 1 &&
			a.count(`SELECT COUNT(*) FROM git_sync_events`) > before
	})
	if got := a.str(`SELECT sha FROM git_sync_events WHERE triggered_by = 'manual' ORDER BY id DESC LIMIT 1`); got != sha2 {
		t.Errorf("the manual sync saw %q, want the connected repository's %q", got, sha2)
	}
	if got := a.str(`SELECT url FROM git_repos WHERE id = ?`, workRepo); got != second {
		t.Errorf("the stored URL is %q, want %q", got, second)
	}
}
