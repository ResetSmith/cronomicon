package gitlab

// What the independent review of Phase R2's registry found, as tests. Each of
// the first four fails on the registry as it was first built.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	plumbing "github.com/go-git/go-git/v5/plumbing"
)

// A Restart is not its caller's to cancel. The caller is the request that
// saved the connection; if that request is abandoned (the page reloaded, a
// proxy timed out), the repository still gets a Service built from its row.
//
// As first built, Restart read the row on the caller's context AFTER it had
// stopped the running Service, and took a read that failed for a repository
// with nothing configured: the repository's Service was replaced by one with no URL
// and no token, and every sync failed "not configured" until the next save.
func TestRegistry_ARestartIsNotItsCallersToCancel(t *testing.T) {
	f := newRegFixture(t)
	if err := f.reg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first sync", func() bool { return f.lastSHA(fixtureRepo) != "" })
	f.reg.Service(fixtureRepo).wg.Wait()
	events := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`)

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.reg.Restart(gone, fixtureRepo); err != nil {
		t.Fatalf("restart on a request that was abandoned: %v", err)
	}
	now := f.reg.Service(fixtureRepo)
	if now == nil {
		t.Fatal("no Service for the repository after the restart")
	}
	if now.repoURL != f.remote {
		t.Errorf("the Service built by the restart has URL %q, want the row's %q", now.repoURL, f.remote)
	}
	eventually(t, "the restarted Service's sync", func() bool {
		return grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events WHERE status = 'success'`) > events
	})
}

// A Restart that cannot READ the repository's row fails, and the repository
// keeps the Service it has. "Could not read" is not "not configured".
func TestRegistry_ARestartThatCannotReadKeepsTheRunningService(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	if err := f.reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first sync", func() bool { return f.lastSHA(fixtureRepo) != "" })
	running := f.reg.Service(fixtureRepo)
	running.wg.Wait()

	// The table is out of reach (what a read error looks like from here).
	if _, err := f.db.Exec(`ALTER TABLE git_repos RENAME TO git_repos_away`); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Restart(ctx, fixtureRepo); err == nil {
		t.Errorf("a restart that could not read the repository's row reported success")
	}
	if got := f.reg.Service(fixtureRepo); got != running {
		t.Errorf("the running Service was replaced although the row could not be read (now %p, was %p)", got, running)
	}
	running.mu.Lock()
	stopped := running.stopped
	running.mu.Unlock()
	if stopped {
		t.Errorf("the running Service was stopped although nothing replaced it")
	}
	if _, err := f.db.Exec(`ALTER TABLE git_repos_away RENAME TO git_repos`); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Restart(ctx, fixtureRepo); err != nil {
		t.Errorf("restart once the row can be read again: %v", err)
	}
	if got := f.reg.Service(fixtureRepo); got == nil || got == running || got.repoURL != f.remote {
		t.Errorf("after the restart that could read the row: %+v", got)
	}
	// A repository that does not exist is refused the same way, and nothing is
	// registered for it.
	if err := f.reg.Restart(ctx, "no-such-repository"); err == nil || f.reg.Service("no-such-repository") != nil {
		t.Errorf("a repository with no row: err=%v, registered=%v", err, f.reg.Service("no-such-repository") != nil)
	}
}

// Restarts of one repository that overlap leave ONE Service, built from the
// row as it is at the end, and every Service they replaced is stopped. (As
// first built, the third of three overlapping restarts could be overwritten
// without being stopped, and the Service left registered could be the one
// built from an older read of the row.)
func TestRegistry_OverlappingRestartsLeaveOneService(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	second := t.TempDir()
	repo2, err := gogit.PlainInit(second, false)
	if err != nil {
		t.Fatal(err)
	}
	grConfinedTo.Store(second, fixtureScope) // the same agency's repository
	gitCommitFile(t, repo2, second, "jobs/other.yaml", jobYAML("other"), "the second repository's job")
	urls := []string{f.remote, second}
	if err := f.reg.Start(ctx); err != nil {
		t.Fatal(err)
	}

	var seen sync.Map
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.db.Exec(`UPDATE git_repos SET url = ? WHERE id = 'repo-a'`, urls[i%2]); err != nil {
				t.Errorf("write the connection: %v", err)
				return
			}
			if err := f.reg.Restart(ctx, fixtureRepo); err != nil {
				t.Errorf("restart: %v", err)
			}
			if svc := f.reg.Service(fixtureRepo); svc != nil {
				seen.Store(svc, true)
			}
		}(i)
	}
	wg.Wait()

	final := f.reg.Service(fixtureRepo)
	if final == nil {
		t.Fatal("no Service after the restarts")
	}
	if want := grString(t, f.db, `SELECT url FROM git_repos WHERE id = 'repo-a'`); final.repoURL != want {
		t.Errorf("the Service left registered has URL %q; the row says %q", final.repoURL, want)
	}
	seen.Range(func(k, _ any) bool {
		svc := k.(*Service)
		svc.mu.Lock()
		stopped := svc.stopped
		svc.mu.Unlock()
		if svc != final && !stopped {
			t.Errorf("a Service that was replaced is still running (URL %q)", svc.repoURL)
		}
		if svc == final && stopped {
			t.Errorf("the Service left registered is stopped")
		}
		return true
	})
	// And the queue is free once everything has unwound.
	f.reg.Close()
	select {
	case f.reg.gate <- struct{}{}:
		<-f.reg.gate
	default:
		t.Errorf("the queue is still held after every Service was stopped")
	}
}

// cancelOnLog is a log handler that cancels a context when a message arrives.
// A sync logs from inside its transaction, which makes this a way to cancel a
// sync exactly there without a hook in the code.
type cancelOnLog struct {
	slog.Handler
	needle string
	cancel func()
}

func (h cancelOnLog) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.needle) {
		h.cancel()
	}
	return nil
}
func (h cancelOnLog) Enabled(context.Context, slog.Level) bool { return true }

// A sync that is cancelled inside its transaction does not reach the
// scheduler. The hook after a sync is the scheduler's reload, which removes
// every cron entry and reads them back: called on a cancelled context it
// removes them and reads nothing, and since a rolled-back sync changed
// neither the commit nor the tables, nothing tells it to try again. Every
// schedule stops firing.
//
// A sync is cancelled there whenever its Service is stopped mid-sync (which
// since Phase R2 is whenever a connection is saved during one) or its caller
// goes away (a scope resync, in the released code too: present defect 29).
// The hook is called only after a commit, and on a context that cannot end.
func TestACancelledSyncDoesNotReachTheScheduler(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	var calls, dead atomic.Int32
	svc.SetOnSyncComplete(func(ctx context.Context, _ string) {
		calls.Add(1)
		if ctx.Err() != nil {
			dead.Add(1)
		}
	})

	// A sync that commits: the hook is called, once.
	if r := svc.SyncBlocking(context.Background(), "manual"); r.Status == "failed" {
		t.Fatalf("first sync: %s", r.ErrorMessage)
	}
	if calls.Load() != 1 || dead.Load() != 0 {
		t.Fatalf("after a sync that committed: %d hook call(s), %d on a dead context; want 1 and 0", calls.Load(), dead.Load())
	}

	// A sync whose caller goes away while it is inside its transaction. The
	// job upsert logs that a job names an SSH credential that is not stored;
	// the context is cancelled on that line. (Until 2.5.3 the line was a job's
	// of Global's repository declaring no scope; an agency's job names one.)
	gitCommitFile(t, repo, remote, "jobs/late.yaml", jobYAML("late")+"  ssh_credential: no-such-credential\n", "a job the cancelled sync will not import")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quiet := svc.log
	svc.log = slog.New(cancelOnLog{Handler: quiet.Handler(), needle: "names no stored SSH credential", cancel: cancel})
	res := svc.SyncBlocking(ctx, "manual")
	svc.log = quiet
	if ctx.Err() == nil {
		t.Fatalf("the sync was not cancelled inside its transaction (no job logged its missing SSH credential); status %q", res.Status)
	}
	if res.Status == "success" || jobCount(t, svc.db, "late") != 0 {
		t.Fatalf("the cancelled sync committed: status %q, late=%d", res.Status, jobCount(t, svc.db, "late"))
	}
	if calls.Load() != 1 {
		t.Errorf("the hook was called %d time(s) for a sync that rolled back; want it not called", calls.Load()-1)
	}
	if dead.Load() != 0 {
		t.Errorf("the hook was handed a cancelled context %d time(s)", dead.Load())
	}

	// The next sync, on a caller that stays, commits and calls the hook; and a
	// caller that leaves AFTER the commit still gets the hook on a live context.
	if r := svc.SyncBlocking(context.Background(), "manual"); r.Status == "failed" || jobCount(t, svc.db, "late") != 1 {
		t.Fatalf("the sync after the cancelled one: status %q, late=%d", r.Status, jobCount(t, svc.db, "late"))
	}
	if calls.Load() != 2 || dead.Load() != 0 {
		t.Errorf("after the next sync: %d hook call(s), %d on a dead context; want 2 and 0", calls.Load(), dead.Load())
	}
}

// A sync cut short because its own Service was stopped leaves no line in the
// history: nothing went wrong, and the Service that replaces it syncs at once.
func TestASyncCutShortByItsServiceBeingStoppedIsNotHistory(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	f.reg.SetOnSyncComplete(func(context.Context, string) { calls.Add(1) })
	if err := f.reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first sync", func() bool { return f.lastSHA(fixtureRepo) != "" })
	svc := f.reg.Service(fixtureRepo)
	svc.wg.Wait()
	events, hooks := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`), calls.Load()

	gitCommitFile(t, f.repo, f.remote, "jobs/late.yaml", jobYAML("late"), "a job the stopped sync will not import")
	// The connection is saved while the sync is past its fetch: the registry
	// halts the Service, and does not wait for it.
	svc.afterFetch = func() { svc.halt() }
	res := svc.SyncBlocking(ctx, "manual")
	if res.Status == "success" || jobCount(t, f.db, "late") != 0 {
		t.Fatalf("the stopped sync committed: status %q, late=%d", res.Status, jobCount(t, f.db, "late"))
	}
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`); n != events {
		t.Errorf("the stopped sync wrote %d line(s) of history: %s", n-events,
			grString(t, f.db, `SELECT status || ': ' || COALESCE(error_message, '') FROM git_sync_events ORDER BY id DESC LIMIT 1`))
	}
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM activity WHERE kind = 'gitsync'`); n != events {
		t.Errorf("the activity stream has %d sync line(s), want the %d there were", n, events)
	}
	if calls.Load() != hooks {
		t.Errorf("the stopped sync called the scheduler's hook")
	}
}

// A connection saved with NO URL has nothing to sync and records nothing; at
// start-up the sync runs and records "not configured", once, as it always did.
func TestRegistry_AConnectionWithNoURLIsNotSynced(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	if _, err := f.db.Exec(`UPDATE git_repos SET url = '' WHERE id = 'repo-a'`); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the start-up sync of a repository with no URL", func() bool {
		return grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events WHERE status = 'failed'`) == 1
	})
	f.reg.Service(fixtureRepo).wg.Wait()
	for i := 0; i < 3; i++ {
		if err := f.reg.Restart(ctx, fixtureRepo); err != nil {
			t.Fatal(err)
		}
		f.reg.Service(fixtureRepo).wg.Wait()
	}
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`); n != 1 {
		t.Errorf("history after three saves of a connection with no URL = %d lines, want the one from start-up", n)
	}
	// It still answers: a manual sync says what is wrong.
	if r := f.reg.Service(fixtureRepo).SyncBlocking(ctx, "manual"); r.Status != "failed" || !strings.Contains(r.ErrorMessage, "not configured") {
		t.Errorf("a manual sync with no URL: %q %q", r.Status, r.ErrorMessage)
	}
}

// sysGit runs the git binary (the file transport go-git uses for a local
// repository needs it anyway).
func sysGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareFixture is a Service whose remote is a BARE repository, so that a publish
// can push to it, with two branches: the first, and `release` with one more job.
func bareFixture(t *testing.T) (svc *Service, bare, first string) {
	t.Helper()
	svc, repo, work := newSyncFixture(t)
	first = svc.Cfg.GitLabWriteBranch
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("release"), Create: true}); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repo, work, "jobs/released.yaml", jobYAML("released"), "on release")
	bare = filepath.Join(t.TempDir(), "bare.git")
	sysGit(t, work, "clone", "--bare", work, bare)
	svc.repoURL = bare
	return svc, bare, first
}

// A publish commits on the connection's branch and pushes it: on a fresh
// clone, on an existing clone, and after the connection's branch has changed,
// when it lands on the NEW branch, on top of that branch's head, and leaves
// the old branch where it was. (No test performed a publish before this one.
// cloneOrFetch now sets the local branch and HEAD itself, and this is what
// that is for.)
func TestPublishLandsOnTheConnectionsBranch(t *testing.T) {
	svc, bare, first := bareFixture(t)
	ctx := context.Background()

	r := svc.SyncBlocking(ctx, "manual")
	if r.Status == "failed" {
		t.Fatalf("first sync: %s", r.ErrorMessage)
	}
	res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/p1.yaml", Content: grScopedJob("p1", fixtureScope)}, r.SHA, "a@example.com")
	if err != nil {
		t.Fatalf("publish on a fresh clone: %v", err)
	}
	if got := sysGit(t, bare, "rev-parse", "refs/heads/"+first); got != res.CommitSHA {
		t.Errorf("after a publish on a fresh clone the remote's %s is %s, want the published %s", first, got, res.CommitSHA)
	}

	r = svc.SyncBlocking(ctx, "manual")
	if r.Status == "failed" || r.SHA != res.CommitSHA {
		t.Fatalf("the sync after the publish: %+v", r)
	}
	res2, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/p2.yaml", Content: grScopedJob("p2", fixtureScope)}, r.SHA, "a@example.com")
	if err != nil {
		t.Fatalf("publish on an existing clone: %v", err)
	}
	if got := sysGit(t, bare, "rev-parse", "refs/heads/"+first); got != res2.CommitSHA {
		t.Errorf("after a publish on an existing clone the remote's %s is %s, want %s", first, got, res2.CommitSHA)
	}
	if got := sysGit(t, bare, "log", "--format=%P", "-1", res2.CommitSHA); got != res.CommitSHA {
		t.Errorf("the second publish's parent is %s, want the first publish %s", got, res.CommitSHA)
	}

	// The connection's branch changes.
	firstHead := sysGit(t, bare, "rev-parse", "refs/heads/"+first)
	releaseHead := sysGit(t, bare, "rev-parse", "refs/heads/release")
	setBranch(t, svc, "release")
	r = svc.SyncBlocking(ctx, "manual")
	if r.Status == "failed" || r.SHA != releaseHead {
		t.Fatalf("the sync on the new branch: %+v, want %s", r, releaseHead)
	}
	res3, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/p3.yaml", Content: grScopedJob("p3", fixtureScope)}, r.SHA, "a@example.com")
	if err != nil {
		t.Fatalf("publish after the branch change: %v", err)
	}
	if got := sysGit(t, bare, "rev-parse", "refs/heads/release"); got != res3.CommitSHA {
		t.Errorf("after the branch change the remote's release is %s, want the published %s", got, res3.CommitSHA)
	}
	if got := sysGit(t, bare, "rev-parse", "refs/heads/"+first); got != firstHead {
		t.Errorf("the publish to release moved the remote's %s to %s", first, got)
	}
	if got := sysGit(t, bare, "log", "--format=%P", "-1", res3.CommitSHA); got != releaseHead {
		t.Errorf("the published commit's parent is %s, want release's head %s", got, releaseHead)
	}
	tree := sysGit(t, bare, "ls-tree", "-r", "--name-only", res3.CommitSHA)
	if !strings.Contains(tree, "jobs/p3.yaml") || !strings.Contains(tree, "jobs/released.yaml") ||
		strings.Contains(tree, "jobs/p1.yaml") || strings.Contains(tree, "jobs/p2.yaml") {
		t.Errorf("the tree published to release:\n%s\nwant release's files and p3, and nothing of the first branch's publishes", tree)
	}
}

// A publish that delivers nothing is a failure, and leaves nothing behind
// (2.4.0; present defect 30, found by the review of Phase R2, the owner's
// decision of 2026-10-10). Until 2.4.0 a publish said it had succeeded and
// pushed nothing when the connection's branch had no local branch in the
// clone: the push's refspec matched nothing, go-git answered "already up to
// date", and that was read as success. The commit was made on whatever branch
// the clone was on and was thrown away by the next successful sync. This was
// the pin TestGR0_APublishToABranchTheCloneDoesNotHaveReportsSuccess.
//
// Two ways there, and both are errors now. A branch the repository does not
// have is found by the fetch a publish starts with, before anything is
// written. And a branch the repository has, with no local branch of that name
// to push (made here by removing it from under the publish), is the push's
// "already up to date" after a commit: an error, with the commit taken back
// off the clone.
func TestAPublishThatDeliversNothingIsAFailure(t *testing.T) {
	svc, bare, first := bareFixture(t)
	ctx := context.Background()
	r := svc.SyncBlocking(ctx, "manual")
	if r.Status == "failed" {
		t.Fatal(r.ErrorMessage)
	}
	before := sysGit(t, bare, "for-each-ref")
	cloneHead := func() string { return sysGit(t, svc.cloneDir, "rev-parse", "HEAD") }
	clean := func(what string) {
		t.Helper()
		if got := cloneHead(); got != r.SHA {
			t.Errorf("%s: the clone's head is %s, want it where the last sync left it, %s", what, got, r.SHA)
		}
		if st := sysGit(t, svc.cloneDir, "status", "--porcelain"); st != "" {
			t.Errorf("%s: the clone is not clean:\n%s", what, st)
		}
		if after := sysGit(t, bare, "for-each-ref"); after != before {
			t.Errorf("%s: the publish reached the remote:\n%s", what, after)
		}
	}

	// A branch the repository does not have.
	setBranch(t, svc, "no-such-branch")
	if bad := svc.SyncBlocking(ctx, "manual"); bad.Status != "failed" {
		t.Fatalf("a sync of a branch the repository does not have: %q", bad.Status)
	}
	res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/p1.yaml", Content: grScopedJob("p1", fixtureScope)}, r.SHA, "a@example.com")
	if err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Fatalf("a publish to a branch the repository does not have answered %+v, %v; want the fetch's error", res, err)
	}
	clean("a branch the repository does not have")

	// The repository's own branch, and nothing to push when the push comes:
	// the local branch is gone by then.
	setBranch(t, svc, first)
	svc.beforePush = func() {
		sysGit(t, svc.cloneDir, "update-ref", "-d", "refs/heads/"+first)
	}
	res, err = svc.Publish(ctx, PublishRequest{FilePath: "jobs/p2.yaml", Content: grScopedJob("p2", fixtureScope)}, r.SHA, "a@example.com")
	svc.beforePush = nil
	if err == nil {
		t.Fatalf("a publish whose push delivered nothing answered success: %+v", res)
	}
	if !strings.Contains(err.Error(), "git push") {
		t.Errorf("the error is %v, want the push's", err)
	}
	if after := sysGit(t, bare, "for-each-ref"); after != before {
		t.Errorf("the publish reached the remote:\n%s", after)
	}
	if _, serr := os.Stat(filepath.Join(svc.cloneDir, "jobs", "p2.yaml")); serr == nil {
		t.Errorf("the failed publish left its file in the clone")
	}
	clean("a push that delivered nothing")
	// Nothing is left in the way of the next publish.
	if _, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/p2.yaml", Content: grScopedJob("p2", fixtureScope)}, r.SHA, "a@example.com"); err != nil {
		t.Fatalf("the publish after the failed one: %v", err)
	}
}
