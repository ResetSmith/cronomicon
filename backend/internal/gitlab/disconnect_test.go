package gitlab

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Disconnecting a repository removes every row it supplied, and no other
// repository's (GR-28): its Git jobs and workflows with their schedule
// entries, its scripts, its Git schedules, its scopes with their host
// membership and imported host records, its sync problems, its connection and
// its clone. It is refused while one of its scopes is bound to runners and has
// work queued, and then removes nothing. A job built in the app on one of its
// scripts stays, joined to nothing. The scheduler is told.
func TestDisconnectingARepositoryRemovesWhatItSuppliedAndNothingElse(t *testing.T) {
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(t.TempDir(), "git-cache", "job-definitions"))
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "jobs/global-job.yaml", grJob("global-job", "echo g"), "Global's job")
	gitCommitFile(t, repoA, remoteA, "scripts/shared.sh", "#!/bin/bash\necho shared\n", "Global's script")
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a) // repo-b, of ag-b, which owns hosts-of-repo-b
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"jobs/b-job.yaml":       grScopedJob("b-job", "hosts-of-repo-b"),
		"scripts/tool.sh":       "#!/bin/bash\necho tool\n",
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
	}, "the agency's definitions")
	grSync(t, b, "the agency's repository")
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A scope the repository supplied, with a host, an imported host record
	// and a runner bound to it; a workflow; a sync problem; a job built in the
	// app on its script; and the same things of Global's, which must stay.
	exec(`INSERT INTO scopes (id, name, source, repo_id, created_at, synced_at) VALUES
	        ('sc-b-git', 'b-git-hosts', 'git', 'repo-b', 't', 't'), ('sc-g-git', 'g-git-hosts', 'git', 'global', 't', 't')`)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc-b-git', 'b1.example'), ('sc-g-git', 'g1.example')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, port, source, scope_id, synced_at, created_at) VALUES
	        ('h-b', 'b1.example', 22, 'git', 'sc-b-git', 't', 't'), ('h-g', 'g1.example', 22, 'git', 'sc-g-git', 't', 't')`)
	exec(`INSERT INTO workflows (uid, name, source, repo_id, source_path, steps, enabled, synced_at) VALUES
	        ('wf-b', 'b-flow', 'git', 'repo-b', 'workflows/b-flow.yaml', '[]', 1, 't'),
	        ('wf-g', 'g-flow', 'git', 'global', 'workflows/g-flow.yaml', '[]', 1, 't')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES
	        ('git', 'workflow', 'b-flow', 'nightly', '0 2 * * *', 0, 'wf-b'), ('git', 'workflow', 'g-flow', 'nightly', '0 2 * * *', 0, 'wf-g')`)
	exec(`INSERT INTO git_sync_problems (repo_id, path, kind, severity, message, first_seen, last_seen, pass) VALUES
	        ('repo-b', 'jobs/x.yaml', 'job', 'error', 'bad', 't', 't', 'p'), ('global', 'jobs/y.yaml', 'job', 'error', 'bad', 't', 't', 'p')`)
	tool := grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id = 'repo-b' AND name = 'tool.sh'`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, script_ref, script_uid, scope) VALUES
	        ('j-app', 'built-on-tool', 'cronomicon', 'bash', 't', 'tool.sh', ?, 'hosts-of-repo-b')`, tool)
	exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
	        VALUES ('agent-b', 'agent-b', 'agent', 'offline', 'Linux', '["bash"]', 0, 5, '1.0', 't', 't')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-b-git', 'agent-b', 'agent-b', 't')`)

	reg := NewRegistry(a.db, slog.New(slog.NewTextHandler(io.Discard, nil)), &config.Config{})
	var told []string
	reg.SetOnSyncComplete(func(_ context.Context, generation string) { told = append(told, generation) })
	clone := reg.cloneDir("repo-b")
	if err := os.MkdirAll(filepath.Join(clone, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	globalClone := reg.cloneDir("global")
	if err := os.MkdirAll(globalClone, 0o755); err != nil {
		t.Fatal(err)
	}
	of := func(repo string) settings.RepoCounts {
		t.Helper()
		c, err := settings.CountRepoRows(ctx, a.db, repo)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	before, globalBefore := of("repo-b"), of("global")
	globalProblems := grCount(t, a.db, `SELECT COUNT(*) FROM git_sync_problems WHERE repo_id = 'global'`)
	if before != (settings.RepoCounts{Jobs: 1, Workflows: 1, Scripts: 1, Schedules: 1, Scopes: 1}) {
		t.Fatalf("the fixture's repository supplies %+v", before)
	}
	generation := func() int { return grCount(t, a.db, `SELECT COALESCE(MAX(n), 0) FROM definitions_generation`) }
	genBefore := generation()

	// Global's is never disconnected, and one that is not there is not found.
	if _, err := reg.Disconnect(ctx, "global", "root"); !errors.Is(err, ErrGlobalRepo) {
		t.Errorf("disconnecting Global's repository answered %v", err)
	}
	if _, err := reg.Disconnect(ctx, "no-such-repo", "root"); !errors.Is(err, settings.ErrRepoNotFound) {
		t.Errorf("disconnecting a repository that is not there answered %v", err)
	}

	// Work is queued on its bound scope: refused, naming the scope, and
	// nothing is removed.
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, executor, triggered_by, trigger_kind, created_at, agencies_json)
	        VALUES ('r-wait', 'b-job', 'bash', 'b-git-hosts', 'queued', 'runner', 'u', 'manual', 't', json_array((SELECT name FROM agencies WHERE id = 'ag-b')))`) // B's run (I2: a run's scope is its agency's)
	_, err := reg.Disconnect(ctx, "repo-b", "root")
	var busy *ErrScopesBusy
	if !errors.As(err, &busy) || len(busy.Scopes) != 1 || busy.Scopes[0] != "b-git-hosts" || !strings.Contains(err.Error(), "b-git-hosts") {
		t.Fatalf("disconnecting with work queued on a bound scope answered %v, want it refused naming the scope", err)
	}
	if got := of("repo-b"); got != before {
		t.Fatalf("the refused disconnect removed rows: %+v, was %+v", got, before)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM git_repos WHERE id = 'repo-b'`); n != 1 {
		t.Fatalf("the refused disconnect removed the connection")
	}
	if _, serr := os.Stat(clone); serr != nil {
		t.Errorf("the refused disconnect removed the clone")
	}
	if len(told) != 0 || generation() != genBefore {
		t.Errorf("the refused disconnect told the scheduler (%v) or advanced the generation", told)
	}
	// It was refused before anything was stopped: no sync service was stopped
	// for it and started again (there was none, and there is none).
	if reg.Service("repo-b") != nil {
		t.Errorf("the refused disconnect stopped and restarted the repository's sync service; it is to be refused first")
	}

	// The work is gone: it goes through, and says what it removed.
	exec(`DELETE FROM runs WHERE id = 'r-wait'`)
	removed, err := reg.Disconnect(ctx, "repo-b", "root")
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if removed != before {
		t.Errorf("the disconnect says it removed %+v, want %+v", removed, before)
	}
	if got := of("repo-b"); got != (settings.RepoCounts{}) {
		t.Errorf("after the disconnect the repository still supplies %+v", got)
	}
	for what, q := range map[string]string{
		"its connection":             `SELECT COUNT(*) FROM git_repos WHERE id = 'repo-b'`,
		"its sync problems":          `SELECT COUNT(*) FROM git_sync_problems WHERE repo_id = 'repo-b'`,
		"its workflow's schedules":   `SELECT COUNT(*) FROM definition_schedules WHERE owner_uid = 'wf-b'`,
		"its scope's hosts":          `SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc-b-git'`,
		"its scope's host records":   `SELECT COUNT(*) FROM ssh_hosts WHERE id = 'h-b'`,
		"its scope's runner binding": `SELECT COUNT(*) FROM scope_runners WHERE scope_id = 'sc-b-git'`,
	} {
		if n := grCount(t, a.db, q); n != 0 {
			t.Errorf("after the disconnect %s: %d rows are left", what, n)
		}
	}
	if _, serr := os.Stat(clone); !os.IsNotExist(serr) {
		t.Errorf("the repository's clone is still there (%v)", serr)
	}

	// Nothing of Global's went, nor its clone, nor the agency's own scope.
	if got := of("global"); got != globalBefore {
		t.Errorf("Global's repository supplies %+v after the disconnect, was %+v", got, globalBefore)
	}
	for what, q := range map[string]string{
		"Global's workflow's schedules":   `SELECT COUNT(*) FROM definition_schedules WHERE owner_uid = 'wf-g'`,
		"Global's scope's hosts":          `SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'sc-g-git'`,
		"Global's scope's host records":   `SELECT COUNT(*) FROM ssh_hosts WHERE id = 'h-g'`,
		"the agency's scope built in app": `SELECT COUNT(*) FROM scopes WHERE name = 'hosts-of-repo-b'`,
		"the agency":                      `SELECT COUNT(*) FROM agencies WHERE id = 'ag-b'`,
	} {
		if n := grCount(t, a.db, q); n != 1 {
			t.Errorf("after the disconnect %s: %d rows, want the one that was there", what, n)
		}
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM git_sync_problems WHERE repo_id = 'global'`); n != globalProblems || n == 0 {
		t.Errorf("Global's sync problems after the disconnect: %d, were %d", n, globalProblems)
	}
	if _, serr := os.Stat(globalClone); serr != nil {
		t.Errorf("Global's clone went with the other repository's: %v", serr)
	}
	// The job built in the app on its script stays, and is joined to nothing.
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs j WHERE j.uid = 'j-app' AND NOT EXISTS (SELECT 1 FROM scripts s WHERE s.uid = j.script_uid)`); n != 1 {
		t.Errorf("the job built in the app on the repository's script: want it there, with its script gone")
	}
	// The scheduler was told, once, with the generation the disconnect made.
	if len(told) != 1 || generation() != genBefore+1 {
		t.Errorf("the scheduler was told %v and the generation went from %d to %d, want one reload on one more", told, genBefore, generation())
	}
	// Disconnected, it is not there to disconnect again.
	if _, err := reg.Disconnect(ctx, "repo-b", "root"); !errors.Is(err, settings.ErrRepoNotFound) {
		t.Errorf("disconnecting it a second time answered %v", err)
	}
}
