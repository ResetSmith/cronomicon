package gitlab

// Phase R0 of 2.4.0 read these three about the connection and its clone and
// did not run them. They are reproduced here before Phase R2 changes anything:
// each test passes on the code as it is and says what R2 is to make of it. No
// production code changes.

import (
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	plumbing "github.com/go-git/go-git/v5/plumbing"
)

// Present defect 17. The repository's URL is used once, to clone. Every sync
// after that fetches from the clone's `origin`, which keeps the URL it was
// cloned from: change the URL in the connection, restart, and the server goes
// on syncing the OLD repository, reporting success.
func TestGR0_AChangedURLNeverReachesTheClone(t *testing.T) {
	svc, _, _ := newSyncFixture(t)
	firstSHA := grSync(t, svc, "first sync, of the first repository").SHA
	if jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("the first repository's job was not imported")
	}

	// A second repository, with other content; the connection now names it.
	// (What a restart after the URL was changed gives the service.)
	second := t.TempDir()
	repo2, err := gogit.PlainInit(second, false)
	if err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repo2, second, "jobs/other.yaml", jobYAML("other"), "the second repository's job")
	svc.repoURL = second

	res := grSync(t, svc, "sync after the URL was changed")
	switch {
	case jobCount(t, svc.db, "other") == 1:
		t.Errorf("the second repository's job was imported: the configured URL reaches the clone now, and this test is to be inverted (Phase R2, GR-12)")
	case res.SHA != firstSHA || jobCount(t, svc.db, "keep") != 1:
		t.Errorf("after the URL change the sync saw %q (first repository: %q) and keep=%d: neither repository", res.SHA, firstSHA, jobCount(t, svc.db, "keep"))
	default:
		// Today: the sync succeeded, against the repository the connection no longer names.
		if res.Status == "failed" {
			t.Errorf("the sync of the old repository was expected to succeed quietly, got: %s", res.ErrorMessage)
		}
	}
}

// Not in the readers' reports; found on 2026-10-09 while reading the clone for
// Phase R2. The clone is made single-branch, so its `origin` fetches the one
// branch it was cloned at. The branch is a setting, read afresh at every sync
// "so a settings change takes effect without a restart" (V1.1-10): change it,
// and every sync from then on fails, because the new branch is never fetched.
// Nothing but deleting the clone directory by hand repairs it.
func TestGR0_AChangedBranchBreaksEverySyncAfterIt(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
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
	// The connection's branch is changed to it.
	svc.Cfg.GitLabWriteBranch = "release"

	res := svc.SyncBlocking(t.Context(), "manual")
	switch {
	case res.Status != "failed" && jobCount(t, svc.db, "released") == 1:
		t.Errorf("the sync followed the branch change: this is fixed, and the test is to be inverted (Phase R2)")
	case res.Status == "failed" && strings.Contains(res.ErrorMessage, "origin/release"):
		// Today: the branch the connection names is never fetched.
	default:
		t.Errorf("after the branch change: status %q, error %q, released=%d", res.Status, res.ErrorMessage, jobCount(t, svc.db, "released"))
	}
	// And it stays broken: a second attempt is no better.
	if again := svc.SyncBlocking(t.Context(), "manual"); again.Status != "failed" && jobCount(t, svc.db, "released") != 1 {
		t.Errorf("the second sync after the branch change: status %q", again.Status)
	}
}

// Present defect 9, its second half. Every sync writes a line to the activity
// stream that names the repository and the branch. Both are read from two
// columns of the connection that nothing has written since migration 060
// (`project_path`, `branch`), so on every real installation the line says
// repository `infra/job-defs`, branch `main`: the fallbacks, whatever
// repository and branch are configured.
func TestGR0_TheActivityLineNamesARepositoryNobodyConfigured(t *testing.T) {
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
	switch got {
	case "infra/job-defs @ main":
		// Today: neither is what was synced.
	default:
		if strings.HasSuffix(got, " @ trunk") {
			t.Errorf("the activity line names the branch that was synced (%q): this is fixed, and the test is to be inverted (Phase R2)", got)
		} else {
			t.Errorf("the activity line says %q", got)
		}
	}
}
