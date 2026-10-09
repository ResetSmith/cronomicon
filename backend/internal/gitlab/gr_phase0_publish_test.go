package gitlab

import (
	"context"
	"strings"
	"testing"
)

// Present defect 32; pinned, NOT fixed. It passes on the code as it is.
//
// A publish writes a file into the server's clone, commits, and pushes. The
// clone is as fresh as the last sync: the publish does not fetch first. Its
// "has anything changed since you looked?" check (If-Match) compares the
// caller's commit with the CLONE's head, not with the repository's. And the
// push is forced (`+refs/heads/<branch>`). So when someone has pushed to the
// repository since the last sync, a publish based on the clone's head passes
// its check and replaces the branch: the commit that was pushed in between is
// no longer on it, and nothing says so. The next sync then prunes whatever
// that commit had added.
//
// Nothing syncs on a timer, so the gap is as long as the last missed webhook
// delivery. A branch protected against force-pushes refuses the publish
// instead (not tested here: the fixture is a bare repository).
func TestGR0_APublishDiscardsCommitsPushedSinceTheLastSync(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.Status == "failed" {
		t.Fatalf("sync: %s", synced.ErrorMessage)
	}

	// Somebody pushes a job to the repository. No sync follows (the webhook
	// delivery was missed, or there is no webhook).
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "a colleague's commit")
	sysGit(t, work, "push", "-q", "origin", branch)
	colleague := sysGit(t, bare, "rev-parse", "refs/heads/"+branch)
	if colleague == synced.SHA {
		t.Fatalf("the colleague's push did not move the branch")
	}

	// A publish from the app, based on what the app last saw.
	res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/from-the-app.yaml", Content: jobYAML("from-the-app")}, synced.SHA, "a@example.com")
	head := sysGit(t, bare, "rev-parse", "refs/heads/"+branch)
	onBranch := strings.Contains(sysGit(t, bare, "rev-list", "refs/heads/"+branch), colleague)
	switch {
	case err != nil:
		t.Errorf("the publish was refused (%v): this is fixed, and the test is to be inverted", err)
	case onBranch:
		t.Errorf("the colleague's commit is still on the branch after the publish: this is fixed, and the test is to be inverted")
	default:
		// Today: the publish succeeded, the branch is the app's commit on top of
		// what the app last saw, and the colleague's commit is not on it.
		if res == nil || head != res.CommitSHA {
			t.Errorf("the branch is at %s, the publish answered %+v", head, res)
		}
		if parent := sysGit(t, bare, "log", "--format=%P", "-1", head); parent != synced.SHA {
			t.Errorf("the published commit's parent is %s, want the commit of the last sync %s", parent, synced.SHA)
		}
	}
}
