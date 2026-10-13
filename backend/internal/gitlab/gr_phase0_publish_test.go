package gitlab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A publish is based on what the repository's branch is NOW (2.4.0; present
// defect 32, the owner's decision of 2026-10-10).
//
// A publish writes a file into the server's clone, commits, and pushes. The
// clone is as fresh as the last sync, and nothing syncs on a timer. Until
// 2.4.0 the publish did not fetch: its "has anything changed since you
// looked?" check (If-Match) compared the caller's commit with the CLONE's
// head, and the push was forced. So when someone had pushed to the repository
// since the last sync, a publish based on the clone's head passed its check
// and replaced the branch: the commit pushed in between was no longer on it,
// and nothing said so. This file held that as
// TestGR0_APublishDiscardsCommitsPushedSinceTheLastSync.
//
// It fetches first now, and goes ahead only when the caller's base, the
// clone's head and the branch's head on the remote are one commit; and the
// push is not forced.
func TestAPublishIsRefusedWhenTheBranchHasMovedSinceTheLastSync(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.Status == "failed" {
		t.Fatalf("sync: %s", synced.ErrorMessage)
	}
	headOf := func() string { return sysGit(t, bare, "rev-parse", "refs/heads/"+branch) }
	cloneHead := func() string { return sysGit(t, svc.cloneDir, "rev-parse", "HEAD") }
	inClone := func(rel string) bool {
		_, err := os.Stat(filepath.Join(svc.cloneDir, rel))
		return err == nil
	}
	publish := func(file, base string) (*PublishResult, error) {
		return svc.Publish(ctx, PublishRequest{FilePath: "jobs/" + file + ".yaml", Content: jobYAML(file)}, base, "a@example.com")
	}
	refused := func(what string, err error, wantCurrent string, wantUnsynced bool) {
		t.Helper()
		var pe *PreconditionError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: the publish answered %v, want it refused as not based on the branch", what, err)
		}
		if pe.CurrentSHA != wantCurrent || pe.Unsynced != wantUnsynced {
			t.Errorf("%s: refused with current %s, unsynced %v; want current %s (the branch's head on the remote), unsynced %v",
				what, pe.CurrentSHA, pe.Unsynced, wantCurrent, wantUnsynced)
		}
	}

	// A base that is not the branch's head, with nothing pushed since the
	// sync: refused, and the clone is not behind.
	_, err := publish("stale", strings.Repeat("0", 40))
	refused("a base that is not the head", err, synced.SHA, false)

	// Somebody pushes to the repository. No sync follows (the webhook
	// delivery was missed, or there is no webhook).
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "a colleague's commit")
	sysGit(t, work, "push", "-q", "origin", branch)
	colleague := headOf()
	if colleague == synced.SHA {
		t.Fatalf("the colleague's push did not move the branch")
	}

	// A publish from the app, based on what the app last saw: refused, with
	// the branch's head as it is on the remote, and the branch, the clone's
	// head and the clone's files as they were.
	_, err = publish("from-the-app", synced.SHA)
	refused("based on the last sync, the branch moved since", err, colleague, true)
	if got := headOf(); got != colleague {
		t.Fatalf("the branch is at %s after the refused publish, want the colleague's commit %s", got, colleague)
	}
	if got := cloneHead(); got != synced.SHA {
		t.Errorf("the clone's head is %s after the refused publish, want it where the last sync left it, %s", got, synced.SHA)
	}
	if inClone("jobs/from-the-app.yaml") {
		t.Errorf("the refused publish left its file in the clone")
	}
	// Based on the branch's head, which the refusal names, with the clone not
	// yet caught up: still refused, and it says the clone is behind.
	_, err = publish("from-the-app", colleague)
	refused("based on the remote's head, the clone behind", err, colleague, true)

	// The clone catches up, and the same publish goes through, ON TOP of the
	// colleague's commit.
	if r := svc.SyncBlocking(ctx, "manual"); r.Status == "failed" || r.SHA != colleague {
		t.Fatalf("the sync after the colleague's push: %+v", r)
	}
	res, err := publish("from-the-app", colleague)
	if err != nil {
		t.Fatalf("the publish based on the synced head: %v", err)
	}
	if got := headOf(); got != res.CommitSHA {
		t.Errorf("the branch is at %s, the publish answered %s", got, res.CommitSHA)
	}
	if parent := sysGit(t, bare, "log", "--format=%P", "-1", res.CommitSHA); parent != colleague {
		t.Errorf("the published commit's parent is %s, want the colleague's commit %s", parent, colleague)
	}
}

// The push is not forced. Somebody pushes between the publish's fetch and its
// push: the remote refuses the publish's push, their commit stays on the
// branch, the publish is an error, and its commit is not left on the clone.
func TestAPublishThatLosesTheRaceDiscardsNothingAndLeavesNothing(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.Status == "failed" {
		t.Fatalf("sync: %s", synced.ErrorMessage)
	}
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	svc.beforePush = func() {
		sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "pushed in between")
		sysGit(t, work, "push", "-q", "origin", branch)
	}
	res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/raced.yaml", Content: jobYAML("raced")}, synced.SHA, "a@example.com")
	svc.beforePush = nil
	between := sysGit(t, work, "rev-parse", "HEAD")
	if err == nil {
		t.Fatalf("the publish succeeded (%+v) although the branch moved before its push", res)
	}
	var pe *PreconditionError
	if errors.As(err, &pe) || !strings.Contains(err.Error(), "git push") {
		t.Errorf("the publish's error is %v, want the push's refusal", err)
	}
	if got := sysGit(t, bare, "rev-parse", "refs/heads/"+branch); got != between {
		t.Fatalf("the branch is at %s, want the commit pushed in between, %s: the publish's push was forced", got, between)
	}
	if got := sysGit(t, svc.cloneDir, "rev-parse", "HEAD"); got != synced.SHA {
		t.Errorf("the clone's head is %s after the failed publish, want it back at %s", got, synced.SHA)
	}
	if _, serr := os.Stat(filepath.Join(svc.cloneDir, "jobs", "raced.yaml")); serr == nil {
		t.Errorf("the failed publish left its file in the clone")
	}
	if st := sysGit(t, svc.cloneDir, "status", "--porcelain"); st != "" {
		t.Errorf("the clone is not clean after the failed publish:\n%s", st)
	}
	// And the next publish, after a sync, works: nothing was left in the way.
	if r := svc.SyncBlocking(ctx, "manual"); r.Status == "failed" || r.SHA != between {
		t.Fatalf("the sync after the race: %+v", r)
	}
	if _, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/raced.yaml", Content: jobYAML("raced")}, between, "a@example.com"); err != nil {
		t.Fatalf("the publish after the race and a sync: %v", err)
	}
}
