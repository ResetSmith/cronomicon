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

// An in-app job follows its agency's repository's SCHEDULE while it is that
// agency's. Since 1400 (§0a, question 34) an in-app job has an owner of its
// own, and its scope given to another agency does not make the job that
// agency's: it is still its owner's, and goes on following its owner's
// repository's schedule. (Until then its agency was its scope's, and the
// scope's move held its timing back, from the review of Phase R4; a move is a
// global administrator's repair since §0a, IN-7, and the job's runs on the
// other agency's scope are refused in any case, runref.RepoScopeMismatch.)
//
// Its copy of the SCRIPT is another matter, and follows whatever agency its
// scope is in (the owner's decision of 2026-10-13; present defect 46). From
// the review of Phase R4 until then the copy was held back as the timing is.
// That job is refused on the other agency's hosts in any case
// (runref.RepoScopeMismatch), and dispatch reads the body from the script's
// record while the run's type, project marker and entry are the copy's: a
// copy held back ran the script's new body under the copy's old interpreter,
// on a scope of the script's own agency. This was the pin
// TestGR0_AJobWhoseCopyIsNotRefreshedRunsItsScriptsBodyUnderItsOldRunType.
func TestGR4_AnInAppJobsTimingStopsFollowingARepositoryItsAgencyNoLongerOwnsAndItsScriptCopyDoesNot(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "ag-a's repository")
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
	// The whole copy, as the composer and sync write it.
	copyOf := func(job string) string {
		t.Helper()
		return grString(t, a.db, `SELECT run_type || '|' || COALESCE(script, '') || '|' || COALESCE(script_path, '') || '|' || COALESCE(project_root, '') || '|' || content_hash
		                            FROM jobs WHERE uid = ?`, "j-"+job)
	}
	scriptsRecord := func() string {
		t.Helper()
		return grString(t, a.db, `SELECT run_type || '|' || COALESCE(script, '') || '|' || COALESCE(script_path, '') || '|' || COALESCE(project_root, '') || '|' || content_hash
		                            FROM scripts WHERE uid = ?`, script)
	}

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
	// The moved job's copy is, by now, wrong in every column a sync writes: as
	// a copy held back since the move would be once the script had changed
	// its type.
	if _, err := a.db.Exec(`UPDATE jobs SET run_type = 'python', script = 'print("held back")', script_path = 'scripts/old.py',
	                               project_root = 'scripts/old', content_hash = 'stale' WHERE uid = 'j-moves'`); err != nil {
		t.Fatal(err)
	}
	grSync(t, b, "the agency's repository, edited")

	if got, want := state("stays"), hash()+" 0 4 4 4 *"; got != want {
		t.Errorf("the job that is still the agency's is %q after the edit, want it following: %q", got, want)
	}
	// The job is still its owner's: its timing follows, as its script's copy does.
	if got, want := state("moves"), hash()+" 0 4 4 4 *"; got != want {
		t.Errorf("the job whose scope is another agency's now is %q after the repository's edit; want it still its owner's, following: %q", got, want)
	}
	if got := grString(t, a.db, `SELECT owner_agency FROM jobs WHERE uid = 'j-moves'`); got != "ag-b" {
		t.Errorf("the job whose scope moved is %q's, want still ag-b's", got)
	}
	for _, j := range []string{"stays", "moves"} {
		if got, want := copyOf(j), scriptsRecord(); got != want {
			t.Errorf("the copy of the job that %s is %q, want it as its script's record is, %q", j, got, want)
		}
	}
	if got := grString(t, a.db, `SELECT run_type FROM jobs WHERE uid = 'j-moves'`); got != "bash" {
		t.Errorf("the moved job's run type is %q after the sync, want its script's (bash): a run takes its type from this row", got)
	}
}

// A reaction in an agency's repository may watch a definition BUILT IN THE APP
// only when it is that agency's. Another agency's is refused in the words used
// for a name nobody holds; and a Git upstream that only another agency's
// repository has is refused too. Global's repository was held to the same rule
// from §0a until 2.5.3; ag-a's, the first repository here since, is held to it.
func TestGR4_AReactionInAnAgencysRepositoryWatchesItsOwnAgencysDefinitionsOnly(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grSync(t, a, "ag-a's repository")
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

	// Another agency's repository (ag-a's) watches its own agency's definitions
	// only (§0a, IN-8; until 2.5.3 this was Global's repository, and until §0a
	// Global's might watch any agency's): an agency's in-app job is refused in
	// the same words, and ag-a's own in-app job is watched.
	if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, scope, owner_agency) VALUES('app-g', 'g-app', 'cronomicon', 'bash', 't', ?, ?)`, fixtureScope, fixtureAgency); err != nil {
		t.Fatal(err)
	}
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"jobs/global-watches.yaml": reacting("global-watches", "cronomicon", "job", "c-app"),
		"jobs/global-own.yaml":     reacting("global-own", "cronomicon", "job", "g-app"),
	}, "ag-a reacts to another agency's in-app job, and to its own")
	grSync(t, a, "ag-a's repository with reactions")
	if synced(fixtureRepo, "global-watches") {
		t.Errorf("ag-a's repository's job reacting to another agency's in-app job was synced")
	}
	if !synced(fixtureRepo, "global-own") {
		t.Errorf("ag-a's repository's job reacting to ag-a's in-app job was not synced")
	}
	y := said("nobodys.yaml", "no-such-job") // the agency's repository's words for nobody's
	rows = grProblems(t, a, fixtureRepo)
	if x := said("global-watches.yaml", "c-app"); x == "" || x != y {
		t.Errorf("ag-a's reaction to another agency's in-app job is refused in other words than nobody's:\n%s\n%s", x, y)
	}
}

// A repository whose agency is not in the catalog supplies no scope: with no
// agency to give it, the scope is not written, rather than born Global's. Its
// file is told so, and it is not counted among the scopes synced.
func TestGR4_AScopeIsNotBornGlobalsForWantOfItsAgency(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "ag-a's repository")
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

// Confinement, where the review's mutations survived: a scope two agencies
// share is not "its agency's". And since names are per agency (I2), a scope
// name another agency holds is no obstacle: the repository's own inventory of
// that name is its agency's own scope, and its jobs on it are synced (until
// then the file was refused, the name taken, and so were the jobs).
func TestGR4_ConfinementCountsOnlyAScopeThatIsItsAgencysAlone(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "ag-a's repository")
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
		// Its own inventory file of a name that is another agency's scope: its
		// agency's own scope of that name (I2).
		"inventory/cs.ini":     fmt.Sprintf(grScope, "b1", "10.0.0.2"),
		"jobs/on-taken.yaml":   grScopedJob("on-taken", "cs"),
		"jobs/on-shared.yaml":  grScopedJob("on-shared", "shared-hosts"),
		"jobs/on-its-own.yaml": grScopedJob("on-its-own", "hosts-of-repo-b"),
	}, "jobs on a taken name and on a shared scope")
	grSync(t, b, "the agency's repository")
	for name, want := range map[string]bool{"on-taken": true, "on-shared": false, "on-its-own": true} {
		if got := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='repo-b' AND name=?`, name) == 1; got != want {
			t.Errorf("the job %s was synced = %v, want %v", name, got, want)
		}
	}
	// Two scopes cs, each its own repository's and agency's: C's is untouched.
	if got := grString(t, a.db, `SELECT GROUP_CONCAT(repo_id || ':' || owner_agency, ',') FROM (SELECT repo_id, owner_agency FROM scopes WHERE name = 'cs' ORDER BY repo_id)`); got != "repo-b:ag-b,repo-c:ag-c" {
		t.Errorf("the scopes cs are %q, want repo-b:ag-b,repo-c:ag-c", got)
	}
	if got := grString(t, a.db, `SELECT GROUP_CONCAT(h.host) FROM scope_hosts h JOIN scopes sc ON sc.id = h.scope_id WHERE sc.name = 'cs' AND sc.repo_id = 'repo-c'`); got != "c1" {
		t.Errorf("C's scope cs holds %q, want its own host c1", got)
	}
}

// An inventory whose name a scope BUILT IN THE APP holds is its file's error
// too, for any repository's (it was a line in the log and
// a clean success).
func TestGR4_AnInventoryNamedLikeAnInAppScopeIsItsFilesError(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grSync(t, a, "ag-a's repository")
	// The repository's agency's own scope built in the app (a scope's name is
	// unique within its agency, I2; until 2.5.3 this was Global's, and the
	// repository Global's).
	if _, err := a.db.Exec(`INSERT INTO scopes (id, name, source, created_by, created_at) VALUES ('s-app', 'built', 'cronomicon', 'a', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-app', ?)`, fixtureAgency); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repoA, remoteA, "inventory/built.ini", fmt.Sprintf(grScope, "g1", "10.0.0.1"), "an inventory of a name the app holds")
	res := grSync(t, a, "ag-a's repository with the inventory")
	rows := grProblems(t, a, fixtureRepo)
	if res.Status != "partial" || !grHas(rows, "error scope inventory/built.ini:", `the scope name "built" is already in use`) {
		t.Errorf("the sync is %q; want partial with an error against the inventory:\n%s", res.Status, strings.Join(rows, "\n"))
	}
	if got := grString(t, a.db, `SELECT source FROM scopes WHERE name = 'built'`); got != "cronomicon" {
		t.Errorf("the scope built in the app is now source %q", got)
	}
}

// "A job that has a script is never moved to another": an in-app job of an
// agency joined to ANOTHER agency's script (ag-a's; Global's until 2.5.3)
// stays on it when its agency's repository syncs a script of the same name.
func TestGR4_ARejoinNeverMovesAJobThatHasAScript(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "scripts/task.sh", "#!/bin/bash\necho task-of-a\n", "ag-a's script")
	grSync(t, a, "ag-a's repository")
	global := grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id='repo-a' AND name='task.sh'`)
	b, repoB, remoteB := grSecondRepo(t, a)
	if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid, scope)
	                        VALUES('j-b', 'app-b', 'cronomicon', 'bash', 't', 'task.sh', ?, 'hosts-of-repo-b')`, global); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, repoB, remoteB, "scripts/task.sh", "#!/bin/bash\necho task-of-b\n", "the agency's script of the same name")
	grSync(t, b, "the agency's repository")
	if got := grString(t, a.db, `SELECT script_uid FROM jobs WHERE uid = 'j-b'`); got != global {
		t.Errorf("the job that had ag-a's script is now joined to %q; want it left on ag-a's %q", got, global)
	}
}
