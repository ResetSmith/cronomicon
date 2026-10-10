package gitlab

// Phase R4 of 2.4.0, after its review: what the review reproduced and what its
// mutations showed no test held. Each test here fails with the statement it
// guards put back to what it was.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"

	gogit "github.com/go-git/go-git/v5"
)

// An in-app job follows its agency's repository's script and schedule only
// while it IS that agency's. Its scope given to another agency, the
// repository's committers no longer change the body, or the timing, of a job
// that now runs on another agency's hosts: it keeps the copy it has.
//
// As Phase R4 was first built the refresh asked only "is this in-app job
// joined to the script", and a scope move left the join in place.
func TestGR4_AnInAppJobStopsFollowingARepositoryItsAgencyNoLongerOwns(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a) // ag-b owns hosts-of-repo-b
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/tool.sh":       "#!/bin/bash\necho one\n",
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
	}, "the agency's script and schedule")
	grSync(t, b, "the agency's repository")
	script := grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id='repo-b' AND name='tool.sh'`)
	sched := grString(t, a.db, `SELECT uid FROM schedules WHERE repo_id='repo-b' AND name='yearly'`)
	hash := func() string { return grString(t, a.db, `SELECT content_hash FROM scripts WHERE uid = ?`, script) }
	for _, j := range []string{"stays", "moves"} {
		scope := "hosts-of-repo-b"
		if j == "moves" {
			grAgencyScope(t, a.db, "ag-b", "moving-hosts")
			scope = "moving-hosts"
		}
		if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid, content_hash, scope)
		                        VALUES(?, ?, 'cronomicon', 'bash', 't', 'tool.sh', ?, ?, ?)`, "j-"+j, "app-"+j, script, hash(), scope); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
		                        VALUES('cronomicon', 'job', ?, 'yearly', '0 3 1 1 *', 0, 'yearly', ?, ?)`, "app-"+j, "j-"+j, sched); err != nil {
			t.Fatal(err)
		}
	}
	state := func(job string) string {
		t.Helper()
		return grString(t, a.db, `SELECT j.content_hash || ' ' || d.cron FROM jobs j JOIN definition_schedules d ON d.owner_uid = j.uid WHERE j.uid = ?`, "j-"+job)
	}
	before := state("moves")

	// One job's scope is given to another agency. Then the repository edits both.
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-c', 'Agency C', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`DELETE FROM scope_agencies WHERE scope_id = 'id-moving-hosts'`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('id-moving-hosts', 'ag-c')`); err != nil {
		t.Fatal(err)
	}
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/tool.sh":       "#!/bin/bash\necho two\n",
		"schedules/yearly.yaml": grSchedule("0 4 4 4 *"),
	}, "the agency edits its script and schedule")
	grSync(t, b, "the agency's repository, edited")

	if got, want := state("stays"), hash()+" 0 4 4 4 *"; got != want {
		t.Errorf("the job that is still the agency's is %q after the edit, want it following: %q", got, want)
	}
	if got := state("moves"); got != before {
		t.Errorf("the job whose scope is another agency's now is %q after the repository's edit; want it as it was, %q", got, before)
	}
}

// A reaction in an agency's repository may watch a definition BUILT IN THE APP
// only when it is that agency's. Another agency's is refused in the words used
// for a name nobody holds; and a Git upstream that only another agency's
// repository has is refused too. Global's repository asks as it always did.
func TestGR4_AReactionInAnAgencysRepositoryWatchesItsOwnAgencysDefinitionsOnly(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	c, repoC, remoteC := grRepo(t, a, "repo-c", "ag-c")
	gitCommitFile(t, repoC, remoteC, "jobs/c-git.yaml", grJob("c-git", "echo c"), "another agency's Git job")
	grSync(t, c, "another agency's repository")
	for _, j := range [][3]string{{"app-b", "b-app", "hosts-of-repo-b"}, {"app-c", "c-app", "hosts-of-repo-c"}} {
		if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, scope) VALUES(?, ?, 'cronomicon', 'bash', 't', ?)`, j[0], j[1], j[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Exec(`INSERT INTO workflows(uid, name, source, steps, enabled, created_at, owner_agency) VALUES
	                        ('wf-b', 'b-flow', 'cronomicon', '[]', 1, 't', 'ag-b'), ('wf-c', 'c-flow', 'cronomicon', '[]', 1, 't', 'ag-c')`); err != nil {
		t.Fatal(err)
	}
	reacting := func(name, onSource, onKind, onName string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  run_type: bash\n  command: echo hi\n" +
			"  reactions:\n    - name: r\n      onSource: " + onSource + "\n      onKind: " + onKind + "\n      onName: " + onName + "\n      onOutcome: success\n"
	}
	files := map[string]string{
		"jobs/own-job.yaml":     reacting("own-job", "cronomicon", "job", "b-app"),
		"jobs/own-flow.yaml":    reacting("own-flow", "cronomicon", "workflow", "b-flow"),
		"jobs/their-job.yaml":   reacting("their-job", "cronomicon", "job", "c-app"),
		"jobs/their-flow.yaml":  reacting("their-flow", "cronomicon", "workflow", "c-flow"),
		"jobs/nobodys.yaml":     reacting("nobodys", "cronomicon", "job", "no-such-job"),
		"jobs/their-git.yaml":   reacting("their-git", "git", "job", "c-git"),
		"jobs/nobodys-git.yaml": reacting("nobodys-git", "git", "job", "no-such-git-job"),
	}
	grCommitFiles(t, repoB, remoteB, files, "reactions to definitions built in the app")
	res := grSync(t, b, "the agency's repository")
	synced := func(repo, name string) bool {
		t.Helper()
		return grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id = ? AND name = ?`, repo, name) == 1
	}
	for name, want := range map[string]bool{"own-job": true, "own-flow": true, "their-job": false, "their-flow": false, "nobodys": false, "their-git": false, "nobodys-git": false} {
		if got := synced("repo-b", name); got != want {
			t.Errorf("the job %s was synced = %v, want %v (%s)", name, got, want, res.ErrorMessage)
		}
	}
	if got := grString(t, a.db, `SELECT COALESCE(r.on_uid,'') FROM reactions r JOIN jobs j ON j.uid = r.owner_uid WHERE j.name = 'own-job'`); got != "app-b" {
		t.Errorf("the reaction to its own agency's in-app job is pinned to %q, want app-b", got)
	}
	// Another agency's and nobody's are refused in the same words.
	rows := grProblems(t, b, "repo-b")
	said := func(file, name string) string {
		for _, r := range rows {
			if strings.Contains(r, " jobs/"+file+": ") {
				_, msg, _ := strings.Cut(r, file+": ")
				return strings.ReplaceAll(msg, name, "X")
			}
		}
		return ""
	}
	if x, y := said("their-job.yaml", "c-app"), said("nobodys.yaml", "no-such-job"); x == "" || x != y {
		t.Errorf("another agency's in-app job and nobody's are refused differently:\n%s\n%s", x, y)
	}
	if x, y := said("their-git.yaml", "c-git"), said("nobodys-git.yaml", "no-such-git-job"); x == "" || x != y {
		t.Errorf("another agency's Git job and nobody's are refused differently:\n%s\n%s", x, y)
	}

	// Global's repository may watch any agency's definition built in the app.
	gitCommitFile(t, repoA, remoteA, "jobs/global-watches.yaml", reacting("global-watches", "cronomicon", "job", "c-app"), "Global reacts to an agency's in-app job")
	grSync(t, a, "Global's repository with a reaction")
	if !synced("global", "global-watches") {
		t.Errorf("Global's repository's job reacting to an agency's in-app job was not synced")
	}
}

// A repository whose agency is not in the catalog supplies no scope: with no
// agency to give it, the scope is not written, rather than born Global's. Its
// file is told so, and it is not counted among the scopes synced.
func TestGR4_AScopeIsNotBornGlobalsForWantOfItsAgency(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	// A repository row may name an agency that is not there: nothing in the
	// schema says otherwise, and the route that will refuse it is Phase R6's.
	remote := t.TempDir()
	repo, err := gogit.PlainInit(remote, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-x', 'ag-nowhere', ?, ?)`, remote, a.Cfg.GitLabWriteBranch); err != nil {
		t.Fatal(err)
	}
	x := &Service{db: a.db, log: slog.New(slog.NewTextHandler(io.Discard, nil)), repoURL: remote, repoID: "repo-x", agencyID: "ag-nowhere",
		cloneDir: filepath.Join(t.TempDir(), "clone"), Cfg: &config.Config{GitLabWriteBranch: a.Cfg.GitLabWriteBranch}}
	gitCommitFile(t, repo, remote, "inventory/theirs.ini", fmt.Sprintf(grScope, "x1", "10.0.0.9"), "a scope of a repository whose agency is not there")
	res := x.SyncBlocking(context.Background(), "t")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scopes WHERE name = 'theirs'`); n != 0 {
		t.Errorf("the scope of a repository whose agency is not there was written (and is in %q)",
			grString(t, a.db, `SELECT COALESCE(GROUP_CONCAT(sa.agency_id), '') FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name='theirs'`))
	}
	if res.Status != "partial" || res.ScopesSynced != 0 {
		t.Errorf("the sync is %q with %d scope(s) synced; want partial and none", res.Status, res.ScopesSynced)
	}
	if rows := grProblems(t, x, "repo-x"); !grHas(rows, "error scope inventory/theirs.ini:", "could not be written") {
		t.Errorf("no error against the inventory that was not written:\n%s", strings.Join(rows, "\n"))
	}
}

// Confinement, where the review's mutations survived: a scope name the
// repository carries an inventory for does not count as its own when the name
// is ALREADY another's; and a scope two agencies share is not "its agency's".
func TestGR4_ConfinementCountsOnlyAScopeThatIsItsAgencysAlone(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	c, repoC, remoteC := grRepo(t, a, "repo-c", "ag-c")
	gitCommitFile(t, repoC, remoteC, "inventory/cs.ini", fmt.Sprintf(grScope, "c1", "10.0.0.3"), "another agency's scope")
	grSync(t, c, "another agency's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	// A scope both agencies are in: a row from before a scope had one agency.
	grAgencyScope(t, a.db, "ag-b", "shared-hosts")
	if _, err := a.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('id-shared-hosts', 'ag-c')`); err != nil {
		t.Fatal(err)
	}
	grCommitFiles(t, repoB, remoteB, map[string]string{
		// Its own inventory file of a name that is another agency's scope: the
		// file is refused (the name is taken), so the scope is not its own.
		"inventory/cs.ini":     fmt.Sprintf(grScope, "b1", "10.0.0.2"),
		"jobs/on-taken.yaml":   grScopedJob("on-taken", "cs"),
		"jobs/on-shared.yaml":  grScopedJob("on-shared", "shared-hosts"),
		"jobs/on-its-own.yaml": grScopedJob("on-its-own", "hosts-of-repo-b"),
	}, "jobs on a taken name and on a shared scope")
	grSync(t, b, "the agency's repository")
	for name, want := range map[string]bool{"on-taken": false, "on-shared": false, "on-its-own": true} {
		if got := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='repo-b' AND name=?`, name) == 1; got != want {
			t.Errorf("the job %s was synced = %v, want %v", name, got, want)
		}
	}
	if got := grString(t, a.db, `SELECT repo_id FROM scopes WHERE name = 'cs'`); got != "repo-c" {
		t.Errorf("the scope cs is now %q's", got)
	}
}

// An inventory whose name a scope BUILT IN THE APP holds is its file's error
// too, for Global's repository as for any other (it was a line in the log and
// a clean success).
func TestGR4_AnInventoryNamedLikeAnInAppScopeIsItsFilesError(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	if _, err := a.db.Exec(`INSERT INTO scopes (id, name, source, created_by, created_at) VALUES ('s-app', 'built', 'cronomicon', 'a', 't')`); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repoA, remoteA, "inventory/built.ini", fmt.Sprintf(grScope, "g1", "10.0.0.1"), "an inventory of a name the app holds")
	res := grSync(t, a, "Global's repository with the inventory")
	rows := grProblems(t, a, "global")
	if res.Status != "partial" || !grHas(rows, "error scope inventory/built.ini:", `the scope name "built" is already in use`) {
		t.Errorf("the sync is %q; want partial with an error against the inventory:\n%s", res.Status, strings.Join(rows, "\n"))
	}
	if got := grString(t, a.db, `SELECT source FROM scopes WHERE name = 'built'`); got != "cronomicon" {
		t.Errorf("the scope built in the app is now source %q", got)
	}
}

// The deferred prune, for a WORKFLOW's use of a schedule (the tests of
// 4f2807e had a job beside every workflow), and Global's sync clearing a
// notice whose row is no longer waiting.
func TestGR4_AWorkflowsUseDefersASchedulesPruneToo(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"scripts/shared.sh":     "#!/bin/bash\necho from-global\n",
	}, "Global's schedule and script")
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"jobs/plain.yaml":     grJob("plain", "echo plain"),
		"jobs/uses.yaml":      "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: uses\nspec:\n  script_ref: shared.sh\n",
		"workflows/flow.yaml": "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  scheduleRefs:\n    - yearly\n  steps:\n    - name: s1\n      job: plain\n",
	}, "a workflow, and no job, on Global's schedule")
	grSync(t, b, "the agency's repository")
	sched := grString(t, a.db, `SELECT uid FROM schedules WHERE repo_id='global' AND name='yearly'`)
	script := grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='shared.sh'`)

	gitRemoveFile(t, repoA, "schedules/yearly.yaml", "the schedule leaves Global's repository")
	gitRemoveFile(t, repoA, "scripts/shared.sh", "and the script")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository without them")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM schedules WHERE uid = ?`, sched); n != 1 {
		t.Fatalf("the schedule only a workflow of another repository uses was pruned")
	}
	open := grDeferred(t, a)
	if _, ok := open["schedule:"+sched+":repo-b"]; !ok || len(open) != 2 {
		t.Fatalf("open notices = %v; want the schedule's and the script's, for repo-b", open)
	}
	// The agency's repository syncs, still using both: its notices stay.
	grBackdateAll(t, a)
	grSync(t, b, "the agency's repository, still using them")
	if open := grDeferred(t, a); len(open) != 2 {
		t.Errorf("the agency's sync cleared a notice while its workflow and job still use the rows: %v", open)
	}

	// Global's repository puts the script back. Nothing is waiting for it any
	// more, and it is Global's sync that says so: the agency's has not run.
	gitCommitFile(t, repoA, remoteA, "scripts/shared.sh", "#!/bin/bash\necho from-global\n", "the script returns")
	grSync(t, a, "Global's repository with the script back")
	open = grDeferred(t, a)
	if _, still := open["script:"+script+":repo-b"]; still || len(open) != 1 {
		t.Errorf("after the script returned to Global's repository, open notices = %v; want the schedule's alone", open)
	}
}

// "A job that has a script is never moved to another": an in-app job of an
// agency joined to GLOBAL's script stays on it when its agency's repository
// syncs a script of the same name.
func TestGR4_ARejoinNeverMovesAJobThatHasAScript(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "scripts/task.sh", "#!/bin/bash\necho task-of-global\n", "Global's script")
	grSync(t, a, "Global's repository")
	global := grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='task.sh'`)
	b, repoB, remoteB := grSecondRepo(t, a)
	if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid, scope)
	                        VALUES('j-b', 'app-b', 'cronomicon', 'bash', 't', 'task.sh', ?, 'hosts-of-repo-b')`, global); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repoB, remoteB, "scripts/task.sh", "#!/bin/bash\necho task-of-b\n", "the agency's script of the same name")
	grSync(t, b, "the agency's repository")
	if got := grString(t, a.db, `SELECT script_uid FROM jobs WHERE uid = 'j-b'`); got != global {
		t.Errorf("the job that had Global's script is now joined to %q; want it left on Global's %q", got, global)
	}
}
