package gitlab

// Phase R0 of 2.4.0 read these three about the connection and its clone and
// did not run them. They were reproduced here (as TestGR0_…, in c630401)
// before Phase R2 changed anything. R2 fixed all three, and each test now says
// what is true instead (TestGR2_…), with what it used to pin in its comment.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	plumbing "github.com/go-git/go-git/v5/plumbing"
)

// The clone follows the connection's URL (GR-12): before every fetch its
// `origin` is set to the configured URL, so a Service built for a changed URL
// syncs the repository the connection names.
//
// Until Phase R2 (present defect 17; TestGR0_AChangedURLNeverReachesTheClone
// pinned it) the URL was used once, to clone. Every sync after that fetched
// from the clone's `origin`, which kept the URL it was cloned from: change the
// URL in the connection, restart, and the server went on syncing the OLD
// repository, reporting success.
func TestGR2_AChangedURLReachesTheClone(t *testing.T) {
	svc, _, _ := newSyncFixture(t)
	firstSHA := grSync(t, svc, "first sync, of the first repository").SHA
	if jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("the first repository's job was not imported")
	}

	// A second repository, with other content; the connection now names it.
	// (What the registry gives the repository's new Service after the URL was
	// changed: the same clone directory, another URL.)
	second := t.TempDir()
	repo2, err := gogit.PlainInit(second, false)
	if err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repo2, second, "jobs/other.yaml", jobYAML("other"), "the second repository's job")
	head2, err := repo2.Head()
	if err != nil {
		t.Fatal(err)
	}
	svc.repoURL = second
	grBackdate(t, svc.db)

	res := grSync(t, svc, "sync after the URL was changed")
	if res.SHA != head2.Hash().String() || res.SHA == firstSHA {
		t.Errorf("after the URL change the sync saw %q, want the second repository's %q", res.SHA, head2.Hash())
	}
	if jobCount(t, svc.db, "other") != 1 {
		t.Errorf("the second repository's job was not imported")
	}
	if jobCount(t, svc.db, "keep") != 0 {
		t.Errorf("the first repository's job is still there: the old repository is still what is synced")
	}
	clone, err := gogit.PlainOpen(svc.cloneDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clone.Config()
	if err != nil {
		t.Fatal(err)
	}
	if o := cfg.Remotes["origin"]; o == nil || len(o.URLs) != 1 || o.URLs[0] != second {
		t.Errorf("the clone's origin = %+v, want the one configured URL %q", o, second)
	}
	// The working tree is the second repository's, with nothing of the first left in it.
	if _, err := os.Stat(filepath.Join(svc.cloneDir, "jobs", "keep.yaml")); !os.IsNotExist(err) {
		t.Errorf("the first repository's file is still in the clone (%v)", err)
	}
	// And it stays there: the next sync, with nothing changed, is the same.
	if again := grSync(t, svc, "the next sync"); again.SHA != res.SHA || jobCount(t, svc.db, "other") != 1 {
		t.Errorf("the next sync saw %q with other=%d", again.SHA, jobCount(t, svc.db, "other"))
	}
}

// The clone follows the connection's branch: a branch changed in the
// connection is fetched and checked out, and publish has a local branch of
// that name to commit on.
//
// Until Phase R2 (present defect 26, found while reading the clone for it;
// TestGR0_AChangedBranchBreaksEverySyncAfterIt pinned it) the clone was made
// single-branch and its `origin` went on fetching the one branch it was cloned
// at. The branch is a setting, read afresh at every sync "so a settings change
// takes effect without a restart" (V1.1-10): change it, and every sync from
// then on failed, the new branch never being fetched, until the clone
// directory was deleted by hand.
func TestGR2_AChangedBranchIsFollowed(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	first := svc.Cfg.GitLabWriteBranch
	grSync(t, svc, "first sync, on the first branch")

	// A second branch in the same repository, with a job of its own.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("release"), Create: true}); err != nil {
		t.Fatalf("create the branch: %v", err)
	}
	gitCommitFile(t, repo, remote, "jobs/released.yaml", jobYAML("released"), "a job on the release branch")
	released, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	// The connection's branch is changed to it.
	svc.Cfg.GitLabWriteBranch = "release"

	res := svc.SyncBlocking(t.Context(), "manual")
	if res.Status == "failed" {
		t.Fatalf("the sync after the branch change failed: %s", res.ErrorMessage)
	}
	if res.SHA != released.Hash().String() || jobCount(t, svc.db, "released") != 1 {
		t.Errorf("after the branch change the sync saw %q with released=%d, want the release branch's %q and its job", res.SHA, jobCount(t, svc.db, "released"), released.Hash())
	}
	clone, err := gogit.PlainOpen(svc.cloneDir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := clone.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Name() != plumbing.NewBranchReferenceName("release") || head.Hash() != released.Hash() {
		t.Errorf("the clone's HEAD is %s at %s, want the local branch release at %s (publish commits on HEAD and pushes that branch)", head.Name(), head.Hash(), released.Hash())
	}

	// And back to the first branch: the job that is only on `release` goes.
	svc.Cfg.GitLabWriteBranch = first
	grBackdate(t, svc.db)
	back := svc.SyncBlocking(t.Context(), "manual")
	if back.Status == "failed" {
		t.Fatalf("the sync after changing the branch back failed: %s", back.ErrorMessage)
	}
	if jobCount(t, svc.db, "released") != 0 || jobCount(t, svc.db, "keep") != 1 {
		t.Errorf("back on the first branch: released=%d keep=%d, want 0 and 1", jobCount(t, svc.db, "released"), jobCount(t, svc.db, "keep"))
	}
	// A branch the repository does not have is an error that says so, and the
	// definitions stay as they were.
	svc.Cfg.GitLabWriteBranch = "no-such-branch"
	if gone := svc.SyncBlocking(t.Context(), "manual"); gone.Status != "failed" || !strings.Contains(gone.ErrorMessage, "no-such-branch") {
		t.Errorf("a branch that does not exist: status %q, error %q; want a failure that names it", gone.Status, gone.ErrorMessage)
	}
	if jobCount(t, svc.db, "keep") != 1 {
		t.Errorf("a failed sync removed a definition")
	}
}

// Every sync writes a line to the activity stream that names the repository
// and the branch that were synced.
//
// Until Phase R2 (present defect 9, its second half;
// TestGR0_TheActivityLineNamesARepositoryNobodyConfigured pinned it) both were
// read from two columns of the connection that nothing had written since
// migration 060 (`project_path`, `branch`), so on every real installation the
// line said repository `infra/job-defs`, branch `main`: the fallbacks,
// whatever repository and branch were configured. Those columns went with the
// table in migration 1310, and the line names what the Service synced.
func TestGR2_TheActivityLineNamesWhatWasSynced(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	// A branch that is not `main`, so that the fallback cannot be right by luck.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("trunk"), Create: true}); err != nil {
		t.Fatalf("create the branch: %v", err)
	}
	gitCommitFile(t, repo, remote, "jobs/t.yaml", jobYAML("t"), "a job on trunk")
	svc.Cfg.GitLabWriteBranch = "trunk"
	// "manual": the history row's trigger is CHECKed, and a sync whose history
	// row is refused writes no activity line either.
	if res := svc.SyncBlocking(t.Context(), "manual"); res.Status == "failed" {
		t.Fatalf("sync of the branch trunk: %s", res.ErrorMessage)
	}

	got := grString(t, svc.db, `SELECT repository || ' @ ' || branch FROM activity WHERE kind='gitsync' ORDER BY id DESC LIMIT 1`)
	// The fixture's repository is a directory; it is named by its last element.
	if want := filepath.Base(remote) + " @ trunk"; got != want {
		t.Errorf("the activity line says %q, want %q", got, want)
	}
}

// A repository is named in the activity stream by the path of its URL, and by
// nothing else a URL can carry.
func TestRepoDisplayPath(t *testing.T) {
	for in, want := range map[string]string{
		"": "",
		"https://gitlab.example/infra/job-defs.git":          "infra/job-defs",
		"https://gitlab.example/group/sub/defs":              "group/sub/defs",
		"https://oauth2:s3cret@gitlab.example/org/defs.git/": "org/defs",
		"http://git.internal:8080/defs.git?x=1":              "defs",
		"/srv/git/job-definitions":                           "job-definitions",
		"/srv/git/job-definitions.git/":                      "job-definitions",
	} {
		if got := repoDisplayPath(in); got != want {
			t.Errorf("repoDisplayPath(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(repoDisplayPath(in), "s3cret") {
			t.Errorf("repoDisplayPath(%q) carries the URL's password", in)
		}
	}
}
