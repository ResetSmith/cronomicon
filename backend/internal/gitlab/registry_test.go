package gitlab

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	plumbing "github.com/go-git/go-git/v5/plumbing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// regFixture is a migrated database, a local repository connected as Global's
// (its URL and branch on Global's row of git_repos), and a Registry whose
// clones go under a temporary directory. Nothing is started.
type regFixture struct {
	t      *testing.T
	db     *sql.DB
	reg    *Registry
	repo   *gogit.Repository
	remote string
	branch string
}

func newRegFixture(t *testing.T) *regFixture {
	t.Helper()
	svc, repo, remote := newSyncFixture(t)
	branch := svc.Cfg.GitLabWriteBranch
	if _, err := svc.db.Exec(`UPDATE git_repos SET url = ?, branch = ? WHERE id = 'global'`, remote, branch); err != nil {
		t.Fatalf("connect Global's repository: %v", err)
	}
	// No environment override: the row is the connection, as it is for every
	// installation that saved one.
	reg := NewRegistry(svc.db, slog.New(slog.NewTextHandler(io.Discard, nil)), &config.Config{})
	clones := t.TempDir()
	reg.cloneDir = func(repoID string) string { return filepath.Join(clones, repoID) }
	t.Cleanup(reg.Close)
	return &regFixture{t: t, db: svc.db, reg: reg, repo: repo, remote: remote, branch: branch}
}

// eventually polls until ok() or the deadline.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (f *regFixture) lastSHA(repoID string) string {
	f.t.Helper()
	return grString(f.t, f.db, `SELECT COALESCE(last_sha, '') FROM git_repos WHERE id = ?`, repoID)
}

func headOf(t *testing.T, repo *gogit.Repository) string {
	t.Helper()
	h, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return h.Hash().String()
}

// A connection that is written takes effect without a restart of the server
// (GR-11, and the first of Phase R2's three tests): the registry builds the
// repository's Service again from its row, and the new Service syncs at once.
// The clone follows too (GR-12): it is the same directory, now fetching from
// the new URL.
//
// Until 2.4.0 the one Service resolved the URL and the token when the routes
// were mounted, and a save logged that the change would apply at the next
// restart. And after that restart the clone still fetched from the URL it was
// cloned from (present defect 17).
func TestRegistry_AWrittenConnectionTakesEffectWithoutARestart(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	if err := f.reg.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	first := f.reg.Global()
	if first == nil {
		t.Fatal("no Service for Global's repository after Start")
	}
	eventually(t, "the first sync of the repository as connected at start", func() bool {
		return f.lastSHA(repoid.Global) == headOf(t, f.repo)
	})
	if jobCount(t, f.db, "keep") != 1 {
		t.Fatalf("the first repository's job was not imported")
	}
	if got := grString(t, f.db, `SELECT triggered_by FROM git_sync_events ORDER BY id LIMIT 1`); got != "poll" {
		t.Errorf("the start-up sync is recorded as %q, want poll", got)
	}

	// Another repository, and the connection now names it.
	second := t.TempDir()
	repo2, err := gogit.PlainInit(second, false)
	if err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repo2, second, "jobs/other.yaml", jobYAML("other"), "the second repository's job")
	head2, _ := repo2.Head()
	if _, err := f.db.Exec(`UPDATE git_repos SET url = ?, branch = ? WHERE id = 'global'`, second, head2.Name().Short()); err != nil {
		t.Fatal(err)
	}
	grBackdate(t, f.db)
	if err := f.reg.Restart(ctx, repoid.Global); err != nil {
		t.Fatalf("restart after the connection was written: %v", err)
	}

	eventually(t, "a sync of the repository the connection names now", func() bool {
		return f.lastSHA(repoid.Global) == headOf(t, repo2)
	})
	if jobCount(t, f.db, "other") != 1 {
		t.Errorf("the newly connected repository's job was not imported")
	}
	if jobCount(t, f.db, "keep") != 0 {
		t.Errorf("the job of the repository that is no longer connected is still there")
	}
	now := f.reg.Global()
	if now == nil || now == first {
		t.Fatalf("the registry still answers with the Service that was built for the old connection")
	}
	if now.repoURL != second {
		t.Errorf("the new Service's URL is %q, want the connection's %q", now.repoURL, second)
	}
	// The Service of the old connection is finished: it starts nothing more.
	before := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`)
	first.TriggerSync(ctx, "manual")
	first.wg.Wait()
	if r := first.SyncBlocking(ctx, "manual"); !r.notRun {
		t.Errorf("a blocking sync on the stopped Service ran: %+v", r)
	}
	if after := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`); after != before {
		t.Errorf("the stopped Service recorded %d more sync(s)", after-before)
	}
}

// A repository connected while the server runs gets its first sync when its
// Service is started, with no timer and no webhook (the owner's decision of
// 2026-10-09). Disconnecting it stops the Service.
func TestRegistry_ARepositoryConnectedWhileRunningGetsItsFirstSync(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	if err := f.reg.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, "Global's first sync", func() bool { return f.lastSHA(repoid.Global) != "" })

	second := t.TempDir()
	repo2, err := gogit.PlainInit(second, false)
	if err != nil {
		t.Fatal(err)
	}
	grAgencyScope(t, f.db, "ag-b", "hosts-of-repo-b")
	gitCommitFile(t, repo2, second, "jobs/agency-job.yaml", jobYAML("agency-job")+"  scope: hosts-of-repo-b\n", "an agency's job")
	head2, _ := repo2.Head()
	if err := f.reg.Restart(ctx, "repo-b"); err == nil {
		t.Errorf("a repository with no row was started")
	}
	if _, err := f.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', ?, ?)`, second, head2.Name().Short()); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Restart(ctx, "repo-b"); err != nil {
		t.Fatalf("start the newly connected repository: %v", err)
	}
	eventually(t, "the newly connected repository's first sync", func() bool {
		return f.lastSHA("repo-b") == headOf(t, repo2)
	})
	if got := grString(t, f.db, `SELECT repo_id FROM jobs WHERE source='git' AND name='agency-job'`); got != "repo-b" {
		t.Errorf("the agency's job has repo_id %q, want repo-b", got)
	}
	svc := f.reg.Service("repo-b")
	if svc == nil || svc.repo() != "repo-b" || svc.agency() != "ag-b" {
		t.Fatalf("the registry's Service for the repository: %+v", svc)
	}
	if svc.cloneDir == f.reg.Global().cloneDir {
		t.Errorf("the two repositories share a clone directory: %s", svc.cloneDir)
	}
	if svc.webhookSecret != "" {
		t.Errorf("the agency's repository's Service was handed the start-up webhook secret, which is Global's alone")
	}

	f.reg.Stop("repo-b")
	if f.reg.Service("repo-b") != nil {
		t.Errorf("the registry still has a Service for the disconnected repository")
	}
	if r := svc.SyncBlocking(ctx, "manual"); !r.notRun {
		t.Errorf("the disconnected repository's Service still syncs: %+v", r)
	}
	if f.reg.Global() == nil {
		t.Errorf("stopping one repository stopped Global's")
	}
}

// One operation at a time, across every repository (GR-11): while one
// repository's sync is working, another repository's sync and a publish wait.
//
// Until 2.4.0 nothing held this even for the one repository: a scope resync
// did not look at the flag a webhook's sync set, and a publish held a mutex
// that a running sync did not (present defect 6).
func TestRegistry_OneOperationAtATime(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	// A second repository, so that there are two Services on the one queue.
	second := t.TempDir()
	repo2, err := gogit.PlainInit(second, false)
	if err != nil {
		t.Fatal(err)
	}
	grAgencyScope(t, f.db, "ag-b", "hosts-of-repo-b")
	gitCommitFile(t, repo2, second, "jobs/agency-job.yaml", jobYAML("agency-job")+"  scope: hosts-of-repo-b\n", "an agency's job")
	head2, _ := repo2.Head()
	if _, err := f.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', ?, ?)`, second, head2.Name().Short()); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both repositories' first syncs", func() bool {
		return f.lastSHA(repoid.Global) != "" && f.lastSHA("repo-b") != ""
	})
	a, b := f.reg.Global(), f.reg.Service("repo-b")
	a.wg.Wait()
	b.wg.Wait()

	// Global's next sync stops in the middle, holding the queue.
	inside, letGo := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(letGo) }) }
	// However the test ends, the held sync is let go: the cleanup waits for it.
	defer release()
	a.afterFetch = func() {
		once.Do(func() { close(inside); <-letGo })
	}
	aDone := make(chan SyncResult, 1)
	go func() { aDone <- a.SyncBlocking(ctx, "manual") }()
	<-inside

	bDone := make(chan SyncResult, 1)
	go func() { bDone <- b.SyncBlocking(ctx, "manual") }()
	pubDone := make(chan error, 1)
	go func() {
		_, err := a.Publish(ctx, PublishRequest{FilePath: "jobs/new.yaml", Content: jobYAML("new")}, "not-the-head", "t@example.com")
		pubDone <- err
	}()
	select {
	case r := <-bDone:
		t.Fatalf("the other repository's sync ran while Global's was working: %+v", r)
	case err := <-pubDone:
		t.Fatalf("a publish ran while a sync was working in the clone: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Both are waiting their turn.
	}
	release()
	for i := 0; i < 3; i++ {
		select {
		case r := <-aDone:
			if r.Status == "failed" {
				t.Errorf("Global's sync: %s", r.ErrorMessage)
			}
			aDone = nil
		case r := <-bDone:
			if r.Status == "failed" {
				t.Errorf("the other repository's sync, once it had its turn: %s", r.ErrorMessage)
			}
			bDone = nil
		case err := <-pubDone:
			// It had its turn and was refused on its precondition, as intended.
			if _, ok := err.(*PreconditionError); !ok {
				t.Errorf("the publish, once it had its turn: %v, want the If-Match refusal", err)
			}
			pubDone = nil
		case <-time.After(20 * time.Second):
			t.Fatal("the queued operations did not finish after the first was released")
		}
	}
}

// Stopping a Service cancels what it has in flight and waits for it: a sync
// that is waiting its turn never runs, and records nothing.
func TestRegistry_StopCancelsAQueuedSync(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	// The queue is held (as by another repository's sync) before anything starts.
	f.reg.gate <- struct{}{}
	if err := f.reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	svc := f.reg.Global()
	stopped := make(chan struct{})
	go func() { f.reg.Stop(repoid.Global); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return while the Service's sync was waiting its turn")
	}
	<-f.reg.gate
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`); n != 0 {
		t.Errorf("a sync that never had its turn recorded %d event(s)", n)
	}
	if f.lastSHA(repoid.Global) != "" {
		t.Errorf("a sync that never had its turn wrote a sync state")
	}
	if f.reg.Service(repoid.Global) != nil {
		t.Errorf("the registry still has the stopped Service")
	}
	svc.TriggerSync(ctx, "webhook") // starts nothing
	svc.wg.Wait()
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_events`); n != 0 {
		t.Errorf("a stopped Service synced on a trigger")
	}

	// Closing the registry stops the rest, and it starts nothing afterwards.
	f.reg.Close()
	if err := f.reg.Restart(ctx, repoid.Global); err == nil {
		t.Errorf("a closed registry started a Service")
	}
}

// A trigger that arrives while a sync is running is not lost: one more sync
// follows. The sync it arrived during had already fetched, so without the
// second one the push that caused the trigger would be picked up by nothing
// until the next delivery (no timer polls).
//
// Until 2.4.0 it was dropped. Present defect 28; found on 2026-10-09 while
// building the queue, and not reproduced on the old code, which had no place
// to stand "during a sync".
func TestATriggerDuringASyncIsNotLost(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	var once sync.Once
	svc.afterFetch = func() {
		once.Do(func() {
			// A push lands after the running sync has fetched, and its webhook
			// delivery triggers a sync.
			gitCommitFile(t, repo, remote, "jobs/pushed.yaml", jobYAML("pushed"), "pushed during the sync")
			svc.TriggerSync(context.Background(), "webhook")
		})
	}
	svc.TriggerSync(context.Background(), "webhook")
	svc.wg.Wait()

	if jobCount(t, svc.db, "pushed") != 1 {
		t.Errorf("the job pushed during the sync was not imported: the trigger its push caused was dropped")
	}
	if got := grString(t, svc.db, `SELECT COALESCE(last_sha,'') FROM git_repos WHERE id='global'`); got != headOf(t, repo) {
		t.Errorf("the last synced commit is %q, want the pushed one %q", got, headOf(t, repo))
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM git_sync_events`); n != 2 {
		t.Errorf("syncs recorded = %d, want the one that was running and the one that followed", n)
	}
	// However many triggers arrive during one sync, one more follows, not many.
	var burst sync.Once
	svc.afterFetch = func() {
		burst.Do(func() {
			for i := 0; i < 5; i++ {
				svc.TriggerSync(context.Background(), "webhook")
			}
		})
	}
	svc.TriggerSync(context.Background(), "webhook")
	svc.wg.Wait()
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM git_sync_events`); n != 4 {
		t.Errorf("syncs recorded after a burst of five triggers during one sync = %d, want 4 (two before, the one running, one more)", n)
	}
}

// A repository's clone directory (GR-12): Global's is where the one repository
// always was, adopted in place; every other is beside it, named by its id.
func TestCloneDirFor(t *testing.T) {
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", "")
	if got, want := CloneDirFor(repoid.Global), "/var/lib/cronomicon/git-cache/job-definitions"; got != want {
		t.Errorf("Global's clone = %q, want the directory it always had, %q", got, want)
	}
	if got, want := CloneDirFor(""), DefaultCloneDir(); got != want {
		t.Errorf("an empty id = %q, want Global's %q", got, want)
	}
	if got, want := CloneDirFor("0190abcd-ef01-7000-8000-000000000001"), "/var/lib/cronomicon/git-cache/0190abcd-ef01-7000-8000-000000000001"; got != want {
		t.Errorf("an agency's repository's clone = %q, want %q", got, want)
	}
	// CRONOMICON_GIT_CACHE_DIR names Global's clone directory itself.
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", "/data/git/defs")
	if got := CloneDirFor(repoid.Global); got != "/data/git/defs" {
		t.Errorf("Global's clone with the override = %q, want the override", got)
	}
	if got := CloneDirFor("repo-b"); got != "/data/git/repo-b" {
		t.Errorf("an agency's repository's clone with the override = %q, want it beside Global's", got)
	}
	// An id is one path element, whatever it holds, and never Global's directory.
	for _, id := range []string{"../../etc", "a/b", "..", ".", "/", "defs"} {
		got := CloneDirFor(id)
		if filepath.Dir(got) != "/data/git" {
			t.Errorf("CloneDirFor(%q) = %q: outside the cache directory", id, got)
		}
		if got == "/data/git/defs" || got == "/data/git" {
			t.Errorf("CloneDirFor(%q) = %q: Global's clone, or the cache itself", id, got)
		}
		if strings.Contains(filepath.Base(got), "..") {
			t.Errorf("CloneDirFor(%q) = %q", id, got)
		}
	}
}

var _ = plumbing.HEAD

// A connection whose URL is taken away while the server runs has nothing left
// to say about its files: no sync will come to clear what the last one found,
// so the rows go and the notice resolves when the connection is written.
func TestRegistry_AConnectionWithNoURLForgetsItsProblems(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	gitCommitFile(t, f.repo, f.remote, "jobs/bad.yaml", "apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: bad\n", "a file that does not validate")
	if err := f.reg.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, "the first sync", func() bool { return f.lastSHA(repoid.Global) == headOf(t, f.repo) })
	open := func() int {
		return grCount(t, f.db, `SELECT COUNT(*) FROM notices WHERE kind='git_sync_problems' AND subject='global' AND resolved_at IS NULL`)
	}
	eventually(t, "the notice of the file that does not validate", func() bool { return open() == 1 })
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_problems WHERE repo_id='global'`); n == 0 {
		t.Fatalf("no problem rows after a sync of a file that does not validate")
	}

	if _, err := f.db.Exec(`UPDATE git_repos SET url = '' WHERE id = 'global'`); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Restart(ctx, repoid.Global); err != nil {
		t.Fatalf("restart after the URL was removed: %v", err)
	}
	if n := grCount(t, f.db, `SELECT COUNT(*) FROM git_sync_problems WHERE repo_id='global'`); n != 0 {
		t.Errorf("%d problem row(s) left for a connection with no URL", n)
	}
	if open() != 0 {
		t.Errorf("the notice is still open for a connection with no URL")
	}
}
