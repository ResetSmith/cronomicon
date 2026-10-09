package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// grLocalRepo makes a local repository with one job in it and returns its
// path, its branch and its head commit.
func grLocalRepo(t *testing.T, job string) (path, branch, sha string) {
	t.Helper()
	path = t.TempDir()
	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	rel := "jobs/" + job + ".yaml"
	if err := os.MkdirAll(filepath.Join(path, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + job + "\nspec:\n  run_type: bash\n  command: echo hi\n"
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
	return path, head.Name().Short(), head.Hash().String()
}

// Saving the GitLab connection applies it: the repository it names is synced
// at once, with no restart of the server, and saving another URL syncs that
// one (GR-11 and GR-12, through the route an administrator uses).
//
// Until 2.4.0 the sync service resolved the URL and the token once, when the
// routes were mounted; the save logged "changes take effect on restart"; and
// after the restart the existing clone still fetched from the old URL.
func TestGR2_ASavedConnectionIsSyncedWithoutARestart(t *testing.T) {
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(t.TempDir(), "clone"))
	a := newGRSchedAPI(t)
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
		raw := a.must(http.MethodGet, "/api/v1/git/sync", nil, http.StatusOK)
		var st struct {
			LastSHA string `json:"lastSHA"`
		}
		_ = json.Unmarshal(raw, &st)
		return st.LastSHA
	}

	first, branch1, sha1 := grLocalRepo(t, "from-first")
	a.must(http.MethodPut, "/api/v1/settings/gitlab", connection(first, branch1), http.StatusOK)
	wait("the repository that was just connected to be synced", func() bool { return jobs("from-first") == 1 })
	wait("the sync status to name its commit", func() bool { return status() == sha1 })

	// The connection is changed to another repository.
	second, branch2, sha2 := grLocalRepo(t, "from-second")
	a.must(http.MethodPut, "/api/v1/settings/gitlab", connection(second, branch2), http.StatusOK)
	wait("the newly named repository to be synced", func() bool { return jobs("from-second") == 1 })
	wait("the sync status to name the new repository's commit", func() bool { return status() == sha2 })

	// The manual sync route goes to the service of the connection as it is NOW.
	before := a.count(`SELECT COUNT(*) FROM git_sync_events`)
	a.must(http.MethodPost, "/api/v1/git/sync", nil, http.StatusAccepted)
	wait("the manual sync to be recorded", func() bool {
		return a.count(`SELECT COUNT(*) FROM git_sync_events WHERE triggered_by = 'manual'`) >= 1 &&
			a.count(`SELECT COUNT(*) FROM git_sync_events`) > before
	})
	if got := a.str(`SELECT sha FROM git_sync_events WHERE triggered_by = 'manual' ORDER BY id DESC LIMIT 1`); got != sha2 {
		t.Errorf("the manual sync saw %q, want the connected repository's %q", got, sha2)
	}
	if got := a.str(`SELECT url FROM git_repos WHERE id = 'global'`); got != second {
		t.Errorf("the stored URL is %q, want %q", got, second)
	}
}
