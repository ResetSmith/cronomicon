package gitlab

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func jobYAML(name string) string {
	return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name +
		"\nspec:\n  run_type: bash\n  command: echo hi\n"
}

func gitCommitFile(t *testing.T, repo *gogit.Repository, root, rel, content, msg string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(grInItsAgency(root, rel, content)), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
}

func gitRemoveFile(t *testing.T, repo *gogit.Repository, rel, msg string) {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
}

func jobCount(t *testing.T, pool *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name=? AND source='git'`, name).Scan(&n); err != nil {
		t.Fatalf("count job %s: %v", name, err)
	}
	return n
}

// The repository newSyncFixture builds is an ordinary agency's: the built-in
// agency has none since 2.5.3 (CA-6), so nothing syncs Central's.
const (
	fixtureRepo   = "repo-a"
	fixtureAgency = "ag-a"
	// fixtureScope is the scope ag-a owns, built in the app, that the
	// fixture's jobs are given when they name none (grInItsAgency, GR-14).
	fixtureScope = "hosts-of-" + fixtureRepo
)

// newSyncFixture creates a local git remote with jobs/keep.yaml committed and a
// Service wired to sync from it into a fresh fully-migrated DB. The remote is
// the repository of the agency ag-a (repo-a), connected on its row of
// git_repos: its URL and branch are there, as every connection's are (the
// CRONOMICON_GITLAB_* overrides are read no more, 2.5.3 question 207).
//
// Until 2.5.3 it was Global's repository, its branch set through
// Cfg.GitLabWriteBranch (CRONOMICON_GITLAB_WRITE_BRANCH). Cfg.GitLabWriteBranch
// is still filled in: the other fixtures read the branch from it.
func newSyncFixture(t *testing.T) (*Service, *gogit.Repository, string) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	remote := t.TempDir()
	repo, err := gogit.PlainInit(remote, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	// A job in an agency's repository names one of its agency's scopes (GR-14).
	grAgencyScope(t, pool, fixtureAgency, fixtureScope)
	grConfinedTo.Store(remote, fixtureScope)
	gitCommitFile(t, repo, remote, "jobs/keep.yaml", jobYAML("keep"), "init keep")
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	branch := head.Name().Short()
	if _, err := pool.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES (?, ?, ?, ?)`,
		fixtureRepo, fixtureAgency, remote, branch); err != nil {
		t.Fatalf("the repository %s's row: %v", fixtureRepo, err)
	}

	svc := &Service{
		db:       pool,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		repoURL:  remote,
		repoID:   fixtureRepo,
		agencyID: fixtureAgency,
		cloneDir: filepath.Join(t.TempDir(), "clone"),
		Cfg:      &config.Config{GitLabWriteBranch: branch},
	}
	return svc, repo, remote
}

// setBranch changes the branch on the connection of svc's repository: its row
// of git_repos, which is what sync and publish read (writeBranch). Until 2.5.3
// the tests set Cfg.GitLabWriteBranch, the CRONOMICON_GITLAB_WRITE_BRANCH
// override of Global's repository; it is kept equal for the fixtures that read
// the branch from it.
func setBranch(t *testing.T, svc *Service, branch string) {
	t.Helper()
	if _, err := svc.db.Exec(`UPDATE git_repos SET branch = ? WHERE id = ?`, branch, svc.repo()); err != nil {
		t.Fatalf("the branch of %s: %v", svc.repo(), err)
	}
	svc.Cfg.GitLabWriteBranch = branch
}

// backdate ages every git job's synced_at so the next sync's prune sees them as
// stale — making prune behavior deterministic regardless of sub-second timing.
func backdate(t *testing.T, pool *sql.DB) {
	t.Helper()
	if _, err := pool.Exec(`UPDATE jobs SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// TestSyncPruneSkippedOnParseError is the PP-B2 catalog-wipe regression: when a
// job file fails to parse, the jobs prune must be skipped so a previously-good,
// not-re-synced job is RETAINED rather than deleted.
func TestSyncPruneSkippedOnParseError(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("initial sync failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("keep not imported by initial sync")
	}
	backdate(t, svc.db) // keep is now stale → a clean prune WOULD delete it

	// Corrupt keep.yaml (unknown apiVersion → validation error in parseJobs).
	gitCommitFile(t, repo, remote, "jobs/keep.yaml",
		"apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: keep\n", "corrupt keep")

	r := svc.SyncBlocking(ctx, "t")
	if jobCount(t, svc.db, "keep") != 1 {
		t.Errorf("keep was PRUNED after a parse-failed sync — catalog-wipe not prevented (PP-B2)")
	}
	if r.Status != "partial" {
		t.Errorf("status = %q, want partial (parse errors present)", r.Status)
	}
}

// TestSyncPrunesRemovedJobOnCleanParse confirms the happy-path prune still works:
// a job removed from a cleanly-parsing repo IS pruned, and siblings are retained.
func TestSyncPrunesRemovedJobOnCleanParse(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	gitCommitFile(t, repo, remote, "jobs/temp.yaml", jobYAML("temp"), "add temp")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "temp") != 1 || jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("expected keep+temp imported")
	}
	backdate(t, svc.db)

	// Remove temp.yaml; repo stays valid → clean parse → temp pruned, keep kept.
	gitRemoveFile(t, repo, "jobs/temp.yaml", "remove temp")
	r := svc.SyncBlocking(ctx, "t")
	if r.Status != "success" {
		t.Errorf("status = %q, want success; errs=%s", r.Status, r.ErrorMessage)
	}
	if jobCount(t, svc.db, "temp") != 0 {
		t.Errorf("temp was not pruned on a clean sync")
	}
	if jobCount(t, svc.db, "keep") != 1 {
		t.Errorf("keep was wrongly pruned on a clean sync")
	}
}
