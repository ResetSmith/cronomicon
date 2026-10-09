package gitlab

// Phase R0 of 2.4.0 (GR, a repository per agency): today's behaviour, pinned.
//
// Each TestGR0_* test passed on the 2.3.2 code and names the phase of 2.4.0
// that inverts it. A test a phase has inverted is renamed for that phase
// (TestGR1_…) and says what it used to pin. The pins Phase R3 inverted are in
// two_repos_test.go (TestGR3_…).
//
// There is nothing to take out before two repositories can meet in one
// database. A Service is a URL and a clone directory; what makes the
// repository single is the tables (gitlab_config and git_sync_state hold one
// row each) and the keys sync writes with. Two Services over one pool are what
// the registry of Phase R2 builds, so these tests build exactly that.

import (
	"context"
	"database/sql"
	"fmt"
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

// grSecondRepo makes a second local remote and a Service that syncs it into
// the SAME database as first, from its own clone directory, as a repository of
// its own ("repo-b", belonging to the agency "ag-b"; the first Service is
// Global's).
func grSecondRepo(t *testing.T, first *Service) (*Service, *gogit.Repository, string) {
	t.Helper()
	remote := t.TempDir()
	repo, err := gogit.PlainInit(remote, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	// Its row of git_repos (migration 1310), with its branch: the environment's
	// branch override, which the first Service's fixture uses, is Global's
	// repository's alone (GR-21), so a second repository reads its own row.
	if _, err := first.db.Exec(`INSERT OR REPLACE INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', ?, ?)`,
		remote, first.Cfg.GitLabWriteBranch); err != nil {
		t.Fatalf("the second repository's row: %v", err)
	}
	return &Service{
		db:       first.db,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		repoURL:  remote,
		repoID:   "repo-b",
		agencyID: "ag-b",
		cloneDir: filepath.Join(t.TempDir(), "clone"),
		Cfg:      &config.Config{GitLabWriteBranch: first.Cfg.GitLabWriteBranch},
	}, repo, remote
}

// grCommitFiles writes several files and commits them once.
func grCommitFiles(t *testing.T, repo *gogit.Repository, root string, files map[string]string, msg string) {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
}

// grBackdate ages every Git row of every kind, so that the next sync's prune
// sees them as stale whatever the clock did between the two syncs (the stamp
// has one-second resolution).
func grBackdate(t *testing.T, pool *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`UPDATE jobs      SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`,
		`UPDATE workflows SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`,
		`UPDATE schedules SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`,
		`UPDATE scripts   SET synced_at='2020-01-01T00:00:00Z'`,
		`UPDATE scopes    SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}
}

func grCount(t *testing.T, pool *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func grString(t *testing.T, pool *sql.DB, query string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := pool.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s.String
}

func grSync(t *testing.T, svc *Service, what string) SyncResult {
	t.Helper()
	r := svc.SyncBlocking(context.Background(), "t")
	if r.Status == "failed" {
		t.Fatalf("%s: sync failed: %s", what, r.ErrorMessage)
	}
	return r
}

const (
	grWorkflow = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: nightly\nspec:\n  steps:\n    - name: s1\n      job: keep\n"
	grScope    = "# cronomicon:v1 owner=t\n[web]\n%s ansible_host=%s\n"
)

func grSchedule(cron string) string {
	return "apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: yearly\nspec:\n  cron: \"" + cron + "\"\n"
}

func grJob(name, command string) string {
	return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name +
		"\nspec:\n  run_type: bash\n  command: " + command + "\n"
}

// Two repositories that both hold scripts/deploy.sh hold a script EACH (Phase
// R1, GR-5; this test pinned the opposite until then: one row keyed by the name,
// carrying whichever body synced last). A script is unique by repository and
// name and has a uid, and a job is joined to the script of its OWN repository.
func TestGR1_TwoScriptsOfOneNameAreTwoScripts(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/deploy.sh": "#!/bin/bash\necho from-a\n",
		"jobs/roll-a.yaml":  "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: roll-a\nspec:\n  script_ref: deploy.sh\n",
	}, "a's deploy.sh and the job that uses it")
	grSync(t, a, "first repository")
	script := func(repo, col string) string {
		t.Helper()
		return grString(t, a.db, `SELECT `+col+` FROM scripts WHERE repo_id=? AND name='deploy.sh'`, repo)
	}
	uidA, hashA := script("global", "uid"), script("global", "content_hash")
	if uidA == "" || hashA == "" {
		t.Fatalf("the first repository's script: uid %q, hash %q", uidA, hashA)
	}
	if got := grString(t, a.db, `SELECT script_uid FROM jobs WHERE source='git' AND name='roll-a'`); got != uidA {
		t.Fatalf("roll-a is joined to script %q, want its own repository's %q", got, uidA)
	}
	// Aged, so that the second repository's prune would take the script if it
	// could: the stamp has one-second resolution and the test must not pass by
	// being quick.
	grBackdate(t, a.db)

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/deploy.sh": "#!/bin/bash\necho from-b\n",
		"jobs/roll-b.yaml":  "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: roll-b\nspec:\n  script_ref: deploy.sh\n",
	}, "b's deploy.sh and the job that uses it")
	grSync(t, b, "second repository")

	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scripts WHERE name='deploy.sh'`); n != 2 {
		t.Fatalf("scripts named deploy.sh = %d, want one per repository", n)
	}
	uidB, hashB := script("repo-b", "uid"), script("repo-b", "content_hash")
	if uidB == "" || uidB == uidA {
		t.Errorf("the second repository's script uid = %q (the first's is %q): want its own", uidB, uidA)
	}
	if hashB == hashA {
		t.Errorf("the two scripts carry one body hash: the second repository's body was not stored as its own")
	}
	// The first repository's script is as it was: same uid, same body.
	if got := script("global", "uid"); got != uidA {
		t.Errorf("the first repository's script uid changed from %q to %q", uidA, got)
	}
	if got := script("global", "content_hash"); got != hashA {
		t.Errorf("the first repository's script body was overwritten by the second repository's")
	}
	// Each job uses the script of its own repository, although both wrote the
	// same name.
	if got := grString(t, a.db, `SELECT script_uid FROM jobs WHERE source='git' AND name='roll-b'`); got != uidB {
		t.Errorf("roll-b is joined to script %q, want its own repository's %q", got, uidB)
	}

	// An edit in one repository keeps that script's uid and touches nothing of
	// the other's.
	gitCommitFile(t, repoB, remoteB, "scripts/deploy.sh", "#!/bin/bash\necho from-b, edited\n", "edit b's deploy.sh")
	grSync(t, b, "second repository again")
	if got := script("repo-b", "uid"); got != uidB {
		t.Errorf("an edit gave the second repository's script a new uid: %q, was %q", got, uidB)
	}
	if got := script("repo-b", "content_hash"); got == hashB {
		t.Errorf("the edit did not reach the second repository's script")
	}
	if got := script("global", "content_hash"); got != hashA {
		t.Errorf("an edit in the second repository changed the first repository's script")
	}
}

// Two repositories that both hold schedules/yearly.yaml hold a schedule EACH
// (Phase R1, GR-6; this test pinned the opposite until then: one row keyed by
// (source, name), whose timing was whichever repository synced last, for every
// definition bound to it). A schedule belongs to its repository's agency and is
// unique by source, owner and name, and a definition's entry is tied to the
// schedule of its OWN repository by uid.
func TestGR1_TwoSchedulesOfOneNameAreTwoSchedules(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	boundJob := func(name string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  run_type: bash\n  command: echo hi\n  scheduleRefs:\n    - yearly\n"
	}
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"jobs/bound-a.yaml":     boundJob("bound-a"),
	}, "a's schedule and a job bound to it")
	grSync(t, a, "first repository")
	sched := func(owner, col string) string {
		t.Helper()
		return grString(t, a.db, `SELECT `+col+` FROM schedules WHERE source='git' AND owner_agency=? AND name='yearly'`, owner)
	}
	entry := func(job, col string) string {
		t.Helper()
		return grString(t, a.db, `SELECT `+col+` FROM definition_schedules WHERE owner_source='git' AND owner_name=?`, job)
	}
	uidA := sched("global", "uid")
	if got := sched("global", "repo_id"); got != "global" {
		t.Errorf("the first repository's schedule has repo_id %q, want global", got)
	}
	if got := entry("bound-a", "schedule_uid"); got != uidA {
		t.Fatalf("bound-a's entry is tied to %q, want its own repository's schedule %q", got, uidA)
	}
	cronA := entry("bound-a", "cron")
	// Aged, so that the second repository's prune would take the schedule if it
	// could: the test must not pass by being quick.
	grBackdate(t, a.db)

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"schedules/yearly.yaml": grSchedule("0 4 2 2 *"),
		"jobs/bound-b.yaml":     boundJob("bound-b"),
	}, "b's schedule and a job bound to it")
	grSync(t, b, "second repository")

	if n := grCount(t, a.db, `SELECT COUNT(*) FROM schedules WHERE source='git' AND name='yearly'`); n != 2 {
		t.Fatalf("git schedules named yearly = %d, want one per repository", n)
	}
	uidB := sched("ag-b", "uid")
	if uidB == "" || uidB == uidA {
		t.Errorf("the second repository's schedule uid = %q (the first's is %q): want its own", uidB, uidA)
	}
	if got := sched("ag-b", "repo_id"); got != "repo-b" {
		t.Errorf("the second repository's schedule has repo_id %q, want repo-b", got)
	}
	// Each keeps its own timing.
	if got := sched("global", "cron"); got != "0 3 1 1 *" {
		t.Errorf("the first repository's schedule now fires at %q: the second repository's overwrote it", got)
	}
	if got := sched("ag-b", "cron"); got != "0 4 2 2 *" {
		t.Errorf("the second repository's schedule fires at %q, want its own", got)
	}
	if got := sched("global", "uid"); got != uidA {
		t.Errorf("the first repository's schedule uid changed from %q to %q", uidA, got)
	}
	// And each job is bound to the schedule of its own repository.
	if got := entry("bound-b", "schedule_uid"); got != uidB {
		t.Errorf("bound-b's entry is tied to %q, want its own repository's schedule %q", got, uidB)
	}
	if got := entry("bound-b", "cron"); got == cronA {
		t.Errorf("bound-b fires at the FIRST repository's timing (%q)", got)
	}
}

// Each repository has its own sync state, and a sync event says which
// repository it was.
//
// Until Phase R2 (TestGR0_TheSyncStateIsOneRow pinned it) the state was one
// row whatever the number of repositories, so the second repository's sync
// overwrote the first one's commit, which was then recorded nowhere; and a
// sync event did not say whose it was. Since migration 1310 the state is on
// the repository's own row of git_repos and git_sync_events has repo_id.
func TestGR2_EachRepositoryHasItsOwnSyncState(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	sync := func(svc *Service, what string) SyncResult {
		t.Helper()
		// "manual": the history row's trigger is CHECKed, and this test reads it.
		r := svc.SyncBlocking(context.Background(), "manual")
		if r.Status == "failed" {
			t.Fatalf("%s: sync failed: %s", what, r.ErrorMessage)
		}
		return r
	}
	ra := sync(a, "first repository")
	// A job says which repository it came from, at first sight. Read before the
	// second repository syncs: a job's KEY is still the name alone, and the jobs
	// prune is not bounded by repository, until Phase R3
	// (TestGR0_TwoJobsOfOneNameAreOneRow, TestGR0_ASecondRepositorysSyncDeletesTheFirsts,
	// both inverted since, in two_repos_test.go), so that sync could remove this one.
	if got := grString(t, a.db, `SELECT repo_id FROM jobs WHERE source='git' AND name='keep'`); got != "global" {
		t.Errorf("the first repository's job has repo_id %q, want global", got)
	}

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/other.yaml": grJob("other", "echo other")}, "the second repository")
	rb := sync(b, "second repository")
	if ra.SHA == "" || ra.SHA == rb.SHA {
		t.Fatalf("the two repositories should be at two commits (first %q, second %q)", ra.SHA, rb.SHA)
	}

	if got := grString(t, a.db, `SELECT last_sha FROM git_repos WHERE id = 'global'`); got != ra.SHA {
		t.Errorf("Global's last_sha = %q, want its own %q: the second repository's sync wrote over it", got, ra.SHA)
	}
	if got := grString(t, a.db, `SELECT last_sha FROM git_repos WHERE id = 'repo-b'`); got != rb.SHA {
		t.Errorf("the second repository's last_sha = %q, want %q", got, rb.SHA)
	}
	if st, err := a.GetSyncState(context.Background()); err != nil || st.LastSHA != ra.SHA {
		t.Errorf("the first Service reads its sync state as %q (%v), want its own %q", st.LastSHA, err, ra.SHA)
	}
	if st, err := b.GetSyncState(context.Background()); err != nil || st.LastSHA != rb.SHA {
		t.Errorf("the second Service reads its sync state as %q (%v), want its own %q", st.LastSHA, err, rb.SHA)
	}
	if got := grString(t, a.db, `SELECT GROUP_CONCAT(repo_id || '=' || sha, ' ') FROM (SELECT repo_id, sha FROM git_sync_events ORDER BY id)`); got != "global="+ra.SHA+" repo-b="+rb.SHA {
		t.Errorf("the sync history = %q, want one event per repository, each naming its own", got)
	}
	// Every table of Git rows says which repository (GR-3).
	for _, table := range []string{"git_sync_events", "schedule_pushes", "jobs", "workflows", "scopes", "scripts", "schedules"} {
		if n := grCount(t, a.db, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'repo_id'`, table); n != 1 {
			t.Errorf("%s has no repo_id column", table)
		}
	}
	if got := grString(t, a.db, `SELECT repo_id FROM jobs WHERE source='git' AND name='other'`); got != "repo-b" {
		t.Errorf("the second repository's job has repo_id %q, want repo-b", got)
	}
}

// BenchmarkGR0_SyncUnchanged measures a sync that finds nothing new in a
// repository of 200 jobs and 40 scripts: the cost every repository pays on
// every sync once Phase R2 runs them one at a time through one queue (GR-11).
func BenchmarkGR0_SyncUnchanged(b *testing.B) {
	remote := b.TempDir()
	repo, err := gogit.PlainInit(remote, false)
	if err != nil {
		b.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		b.Fatal(err)
	}
	write := func(rel, content string) {
		abs := filepath.Join(remote, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
		if _, err := wt.Add(rel); err != nil {
			b.Fatal(err)
		}
	}
	for i := range 40 {
		write(fmt.Sprintf("scripts/s%03d.sh", i), fmt.Sprintf("#!/bin/bash\necho script %d\n", i))
	}
	for i := range 200 {
		write(fmt.Sprintf("jobs/j%03d.yaml", i), fmt.Sprintf(
			"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: j%03d\nspec:\n  script_ref: s%03d.sh\n  schedule: \"%d 3 * * *\"\n",
			i, i%40, i%60))
	}
	if _, err := wt.Commit("240 definitions", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		b.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		b.Fatal(err)
	}
	pool, err := db.Open(filepath.Join(b.TempDir(), "sync.db"))
	if err != nil {
		b.Fatalf("open db: %v", err)
	}
	b.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	svc := &Service{
		db:       pool,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		repoURL:  remote,
		cloneDir: filepath.Join(b.TempDir(), "clone"),
		Cfg:      &config.Config{GitLabWriteBranch: head.Name().Short()},
	}
	start := time.Now()
	first := svc.SyncBlocking(context.Background(), "bench")
	if first.Status != "success" {
		b.Fatalf("first sync: %s %s", first.Status, first.ErrorMessage)
	}
	firstMs := float64(time.Since(start).Milliseconds())
	if first.JobsSynced != 200 || first.ScriptsSynced != 40 {
		b.Fatalf("first sync imported %d jobs and %d scripts, want 200 and 40", first.JobsSynced, first.ScriptsSynced)
	}
	b.ResetTimer()
	for b.Loop() {
		if r := svc.SyncBlocking(context.Background(), "bench"); r.Status != "success" {
			b.Fatalf("sync: %s %s", r.Status, r.ErrorMessage)
		}
	}
	// After the loop: ResetTimer discards metrics reported before it.
	b.ReportMetric(firstMs, "first-import-ms")
}
