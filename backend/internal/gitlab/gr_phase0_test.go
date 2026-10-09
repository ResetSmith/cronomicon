package gitlab

// Phase R0 of 2.4.0 (GR, a repository per agency): today's behaviour, pinned.
//
// Each TestGR0_* test passed on the 2.3.2 code and names the phase of 2.4.0
// that inverts it. A test a phase has inverted is renamed for that phase
// (TestGR1_…) and says what it used to pin.
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
// its own ("repo-b"; the first Service is Global's).
func grSecondRepo(t *testing.T, first *Service) (*Service, *gogit.Repository, string) {
	t.Helper()
	remote := t.TempDir()
	repo, err := gogit.PlainInit(remote, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	return &Service{
		db:       first.db,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		repoURL:  remote,
		repoID:   "repo-b",
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

// A second repository's sync deletes what the first one supplied, except its
// scripts. Every prune statement but one asks only "is this a Git row that this
// pass did not stamp?" and not whose repository the row came from. Phase R3
// (GR-13) inverts this for the remaining kinds: a sync prunes only rows of ITS
// repository.
//
// The scripts prune was the worst of them (it did not even ask for a Git row)
// and is the first to be bounded: scripts carry their repository since Phase R1
// (migration 1290), so the first repository's script now SURVIVES.
func TestGR0_ASecondRepositorysSyncDeletesTheFirsts(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t) // jobs/keep.yaml
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"workflows/nightly.yaml": grWorkflow,
		"schedules/yearly.yaml":  grSchedule("0 3 1 1 *"),
		"scripts/deploy.sh":      "#!/bin/bash\necho from-a\n",
		"inventory/web.ini":      fmt.Sprintf(grScope, "web1", "10.0.0.1"),
	}, "the first repository")
	grSync(t, a, "first repository")

	has := map[string]string{
		"job":      `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='keep'`,
		"workflow": `SELECT COUNT(*) FROM workflows WHERE source='git' AND name='nightly'`,
		"schedule": `SELECT COUNT(*) FROM schedules WHERE source='git' AND name='yearly'`,
		"script":   `SELECT COUNT(*) FROM scripts WHERE name='deploy.sh'`,
		"scope":    `SELECT COUNT(*) FROM scopes WHERE source='git' AND name='web'`,
	}
	for kind, q := range has {
		if n := grCount(t, a.db, q); n != 1 {
			t.Fatalf("the first repository's %s was not imported (count %d)", kind, n)
		}
	}
	grBackdate(t, a.db)

	// The second repository holds one unrelated job and nothing else.
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/other.yaml": grJob("other", "echo other")}, "the second repository")
	grSync(t, b, "second repository")

	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='other'`); n != 1 {
		t.Fatalf("the second repository's job was not imported (count %d)", n)
	}
	for kind, q := range has {
		n := grCount(t, a.db, q)
		if kind == "script" {
			if n != 1 {
				t.Errorf("the first repository's script did not survive the second repository's sync (count %d): "+
					"the scripts prune is bounded by repository since Phase R1", n)
			}
			continue
		}
		if n != 0 {
			t.Errorf("the first repository's %s survived the second repository's sync (count %d): "+
				"today's prune is not expected to tell repositories apart. If this is now deliberate, "+
				"Phase R3 has landed and this test is to be inverted", kind, n)
		}
	}
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

// The same for a reusable schedule: the key is (source, name), so two
// repositories' schedules of one name are one row and the last sync decides
// when every definition that names it fires. Phase R1 (GR-6) inverts this:
// uniqueness becomes (source, owner_agency, name).
func TestGR0_TwoSchedulesOfOneNameAreOneRow(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "schedules/yearly.yaml", grSchedule("0 3 1 1 *"), "a's schedule")
	grSync(t, a, "first repository")

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"schedules/yearly.yaml": grSchedule("0 4 2 2 *")}, "b's schedule")
	grSync(t, b, "second repository")

	if n := grCount(t, a.db, `SELECT COUNT(*) FROM schedules WHERE source='git' AND name='yearly'`); n != 1 {
		t.Fatalf("git schedules named yearly = %d, want the one shared row", n)
	}
	if got := grString(t, a.db, `SELECT cron FROM schedules WHERE source='git' AND name='yearly'`); got != "0 4 2 2 *" {
		t.Errorf("the shared schedule's cron = %q, want the second repository's", got)
	}
}

// The same for a job: (source, name) WHERE source='git'. Phase R3 (GR-4)
// inverts this: (repo_id, name).
func TestGR0_TwoJobsOfOneNameAreOneRow(t *testing.T) {
	a, _, _ := newSyncFixture(t) // jobs/keep.yaml: echo hi
	grSync(t, a, "first repository")

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/keep.yaml": grJob("keep", "echo from-b")}, "b's keep")
	grSync(t, b, "second repository")

	if n := jobCount(t, a.db, "keep"); n != 1 {
		t.Fatalf("git jobs named keep = %d, want the one shared row", n)
	}
	if got := grString(t, a.db, `SELECT command FROM jobs WHERE source='git' AND name='keep'`); got != "echo from-b" {
		t.Errorf("the shared job's command = %q, want the second repository's", got)
	}
}

// A scope of one name in two repositories is one scope, and the second
// repository's inventory replaces the first's hosts. Scope names stay unique
// across the installation (I-2), so this one is not inverted but REFUSED from
// Phase R4 on (GR-19): the second file is skipped with "name already in use".
func TestGR0_TwoScopesOfOneNameAreOneScope(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "inventory/web.ini", fmt.Sprintf(grScope, "web1", "10.0.0.1"), "a's scope")
	grSync(t, a, "first repository")
	hosts := `SELECT COUNT(*) FROM scope_hosts WHERE host=? AND scope_id=(SELECT id FROM scopes WHERE name='web')`
	if n := grCount(t, a.db, hosts, "web1"); n != 1 {
		t.Fatalf("the first repository's host was not imported (count %d)", n)
	}

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"inventory/web.ini": fmt.Sprintf(grScope, "web2", "10.0.0.2")}, "b's scope")
	grSync(t, b, "second repository")

	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scopes WHERE name='web'`); n != 1 {
		t.Fatalf("scopes named web = %d, want one", n)
	}
	if n := grCount(t, a.db, hosts, "web2"); n != 1 {
		t.Errorf("the second repository's host is not in the scope (count %d)", n)
	}
	if n := grCount(t, a.db, hosts, "web1"); n != 0 {
		t.Errorf("the first repository's host is still in the scope (count %d): the second inventory was expected to replace it", n)
	}
}

// The sync state is one row whatever the number of repositories, and a sync
// event does not say which repository it was. Phases R2 and R3 invert this:
// the state moves onto git_repos, and git_sync_events gains repo_id.
func TestGR0_TheSyncStateIsOneRow(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	ra := grSync(t, a, "first repository")

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/other.yaml": grJob("other", "echo other")}, "the second repository")
	rb := grSync(t, b, "second repository")
	if ra.SHA == "" || ra.SHA == rb.SHA {
		t.Fatalf("the two repositories should be at two commits (first %q, second %q)", ra.SHA, rb.SHA)
	}

	if n := grCount(t, a.db, `SELECT COUNT(*) FROM git_sync_state`); n != 1 {
		t.Fatalf("git_sync_state rows = %d, want the singleton", n)
	}
	if got := grString(t, a.db, `SELECT last_sha FROM git_sync_state WHERE id = 1`); got != rb.SHA {
		t.Errorf("git_sync_state.last_sha = %q, want the second repository's %q: the first repository's commit is no longer recorded anywhere", got, rb.SHA)
	}
	// scripts left this list with Phase R1 (migration 1290); schedules follow in
	// the second half of R1, the rest in R2.
	for _, table := range []string{"git_sync_events", "schedule_pushes", "jobs", "workflows", "schedules", "scopes"} {
		if n := grCount(t, a.db, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'repo_id'`, table); n != 0 {
			t.Errorf("%s already has a repo_id column: the phase that adds it has landed and this test is to be inverted", table)
		}
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM pragma_table_info('scripts') WHERE name = 'repo_id'`); n != 1 {
		t.Errorf("scripts has no repo_id column: migration 1290 is missing")
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
