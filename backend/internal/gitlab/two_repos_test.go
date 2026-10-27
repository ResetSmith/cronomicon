package gitlab

// Phase R3 of 2.4.0: sync per repository. Two Services over one database, each
// with its own repository and clone, as the registry builds them. Every test
// here inverts a pin of Phase R0 (gr_phase0_test.go) and says which.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// grBackdateAll ages every Git row, the imported host records included, so
// that the next sync's prune sees them as stale whatever the clock did.
func grBackdateAll(t *testing.T, svc *Service) {
	t.Helper()
	grBackdate(t, svc.db)
	if _, err := svc.db.Exec(`UPDATE ssh_hosts SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`); err != nil {
		t.Fatal(err)
	}
}

// A sync prunes what ITS repository no longer has, and nothing of any other
// repository (GR-13).
//
// Until Phase R3 (TestGR0_ASecondRepositorysSyncDeletesTheFirsts pinned it) the
// prune statements asked only "is this a Git row that this pass did not
// stamp?": a second repository's sync deleted every job, workflow and scope the
// first had supplied, and their schedule entries, hosts and host records with
// them. (Scripts and schedules were bounded in Phase R1.)
func TestGR3_ASyncPrunesOnlyItsOwnRepository(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t) // jobs/keep.yaml
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"workflows/nightly.yaml": grWorkflow,
		"schedules/yearly.yaml":  grSchedule("0 3 1 1 *"),
		"scripts/deploy.sh":      "#!/bin/bash\necho from-a\n",
		"inventory/web.ini":      fmt.Sprintf(grScope, "web1", "10.0.0.1"),
		"jobs/timed.yaml":        "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: timed\nspec:\n  run_type: bash\n  command: echo hi\n  scheduleRefs:\n    - yearly\n",
	}, "the first repository")
	grSync(t, a, "first repository")

	firsts := map[string]string{
		"job":                 `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='keep' AND repo_id='repo-a'`,
		"workflow":            `SELECT COUNT(*) FROM workflows WHERE source='git' AND name='nightly' AND repo_id='repo-a'`,
		"schedule":            `SELECT COUNT(*) FROM schedules WHERE source='git' AND name='yearly' AND repo_id='repo-a'`,
		"script":              `SELECT COUNT(*) FROM scripts WHERE name='deploy.sh' AND repo_id='repo-a'`,
		"scope":               `SELECT COUNT(*) FROM scopes WHERE source='git' AND name='web' AND repo_id='repo-a'`,
		"scope's host":        `SELECT COUNT(*) FROM scope_hosts WHERE host='web1'`,
		"host record":         `SELECT COUNT(*) FROM ssh_hosts WHERE source='git' AND hostname='web1'`,
		"job's schedule":      `SELECT COUNT(*) FROM definition_schedules d JOIN jobs j ON j.uid = d.owner_uid WHERE j.name='timed' AND d.name='yearly'`,
		"job's log-folder id": `SELECT COUNT(*) FROM entity_codes e JOIN jobs j ON j.uid = e.uid WHERE j.name='keep' AND e.deleted_at IS NULL AND e.repo_id='repo-a'`,
	}
	check := func(when string) {
		t.Helper()
		for kind, q := range firsts {
			if n := grCount(t, a.db, q); n != 1 {
				t.Errorf("%s: the first repository's %s: count %d, want 1", when, kind, n)
			}
		}
	}
	check("after its own sync")
	// A pause on one of its jobs: a satellite the second repository's sync must leave.
	if _, err := a.db.Exec(`INSERT INTO paused_jobs (name, source, owner_kind, owner_uid, paused_by, paused_at)
	                        SELECT name, 'git', 'job', uid, 'ops@example', 't' FROM jobs WHERE source='git' AND name='keep'`); err != nil {
		t.Fatalf("pause: %v", err)
	}
	firsts["job's pause"] = `SELECT COUNT(*) FROM paused_jobs p JOIN jobs j ON j.uid = p.owner_uid WHERE j.name='keep'`

	// The second repository holds one unrelated job and nothing else: no
	// workflows, schedules, scripts or inventory, so every one of its prunes runs
	// over a kind it has none of.
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/other.yaml": grJob("other", "echo other")}, "the second repository")
	grBackdateAll(t, a)
	if r := grSync(t, b, "second repository"); r.Status != "success" {
		t.Fatalf("the second repository's sync: %s (%s)", r.Status, r.ErrorMessage)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='other' AND repo_id='repo-b'`); n != 1 {
		t.Fatalf("the second repository's job was not imported (count %d)", n)
	}
	check("after the second repository's sync")

	// And the other way round: the first repository's sync leaves the second's job.
	grBackdateAll(t, a)
	grSync(t, a, "first repository again")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='other'`); n != 1 {
		t.Errorf("the first repository's sync removed the second's job (count %d)", n)
	}
	check("after its own second sync")

	// Each still prunes its OWN: the second repository drops its job.
	gitRemoveFile(t, repoB, "jobs/other.yaml", "remove other")
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/another.yaml": grJob("another", "echo another")}, "another")
	grBackdateAll(t, a)
	grSync(t, b, "second repository after a removal")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='other'`); n != 0 {
		t.Errorf("the second repository's removed job is still there (count %d)", n)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='another' AND repo_id='repo-b'`); n != 1 {
		t.Errorf("the second repository's new job: count %d, want 1", n)
	}
	check("after the second repository pruned its own")
}

// Two repositories with the SAME names sync side by side: a job, a script, a
// schedule and a workflow of one name in each are two of each, every one with
// its own schedule entries, reactions, log folder and notice, and neither
// repository's sync touches the other's (the test Phase R3's plan asks for).
//
// Until Phase R3 (TestGR0_TwoJobsOfOneNameAreOneRow pinned the first of these)
// a job's key was (source, name): the second repository's `nightly` WAS the
// first's, overwritten. And what hangs off a job was written by its name, so
// even with two rows each sync deleted the other's schedule entries and
// reactions and wrote its own with no owner uid, and the two jobs took turns
// owning one log folder.
func TestGR3_TwoRepositoriesWithTheSameNames(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	files := func(command, cron string) map[string]string {
		return map[string]string{
			"schedules/yearly.yaml": grSchedule(cron),
			"scripts/task.yaml":     "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: task\nspec:\n  run_type: bash\n  command: " + command + "\n",
			"jobs/nightly.yaml": "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: nightly\nspec:\n  script_ref: task\n  runner_tag: legacy-pin\n" +
				"  scheduleRefs:\n    - yearly\n",
			"jobs/after.yaml": "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: after\nspec:\n  run_type: bash\n  command: echo after\n" +
				"  reactions:\n    - name: when-nightly\n      onKind: job\n      onName: nightly\n      onOutcome: success\n",
			"workflows/flow.yaml": "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  scheduleRefs:\n    - yearly\n  steps:\n    - name: s1\n      job: nightly\n",
		}
	}
	grCommitFiles(t, repoA, remoteA, files("echo from-a", "0 3 1 1 *"), "the first repository")
	if r := grSync(t, a, "first repository"); r.Status != "success" {
		t.Fatalf("the first repository's sync: %s (%s)", r.Status, r.ErrorMessage)
	}
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, files("echo from-b", "0 4 2 2 *"), "the second repository")
	if r := grSync(t, b, "second repository"); r.Status != "success" {
		t.Fatalf("the second repository's sync: %s (%s)", r.Status, r.ErrorMessage)
	}

	str := func(q string, args ...any) string { t.Helper(); return grString(t, a.db, q, args...) }
	count := func(q string, args ...any) int { t.Helper(); return grCount(t, a.db, q, args...) }
	// state is everything about one repository's definitions of these names.
	state := func(repo string) map[string]string {
		t.Helper()
		job := str(`SELECT uid FROM jobs WHERE source='git' AND repo_id=? AND name='nightly'`, repo)
		after := str(`SELECT uid FROM jobs WHERE source='git' AND repo_id=? AND name='after'`, repo)
		wf := str(`SELECT uid FROM workflows WHERE source='git' AND repo_id=? AND name='flow'`, repo)
		return map[string]string{
			"job uid":                         job,
			"job command":                     str(`SELECT command FROM jobs WHERE uid=?`, job),
			"job script":                      str(`SELECT repo_id FROM scripts WHERE uid=(SELECT script_uid FROM jobs WHERE uid=?)`, job),
			"job schedule entries":            fmt.Sprint(count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_kind='job' AND owner_uid=?`, job)),
			"job schedule timing":             str(`SELECT cron FROM definition_schedules WHERE owner_kind='job' AND owner_uid=?`, job),
			"job schedule's schedule":         str(`SELECT s.repo_id FROM definition_schedules d JOIN schedules s ON s.uid = d.schedule_uid WHERE d.owner_kind='job' AND d.owner_uid=?`, job),
			"job log-folder id":               str(`SELECT code || '/' || COALESCE(repo_id,'(none)') FROM entity_codes WHERE kind='job' AND uid=? AND deleted_at IS NULL`, job),
			"job notice":                      fmt.Sprint(count(`SELECT COUNT(*) FROM retired_runner_pins WHERE job_uid=? AND reason='leftover_git_key'`, job)),
			"workflow uid":                    wf,
			"workflow schedule entries":       fmt.Sprint(count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_kind='workflow' AND owner_uid=?`, wf)),
			"workflow log-folder id":          str(`SELECT code || '/' || COALESCE(repo_id,'(none)') FROM entity_codes WHERE kind='workflow' AND uid=? AND deleted_at IS NULL`, wf),
			"reaction rows":                   fmt.Sprint(count(`SELECT COUNT(*) FROM reactions WHERE owner_kind='job' AND owner_uid=?`, after)),
			"reaction watches its repository": fmt.Sprint(str(`SELECT COALESCE(on_uid,'(none)') FROM reactions WHERE owner_kind='job' AND owner_uid=?`, after) == job),
		}
	}
	want := func(repo, command, cron string, got map[string]string) {
		t.Helper()
		for k, v := range map[string]string{
			"job command": command, "job script": repo, "job schedule entries": "1", "job schedule timing": cron,
			"job schedule's schedule": repo, "job notice": "1", "workflow schedule entries": "1",
			"reaction rows": "1", "reaction watches its repository": "true",
		} {
			if got[k] != v {
				t.Errorf("repository %s: %s = %q, want %q", repo, k, got[k], v)
			}
		}
		for _, k := range []string{"job log-folder id", "workflow log-folder id"} {
			if len(got[k]) < len(repo)+2 || got[k][len(got[k])-len(repo)-1:] != "/"+repo {
				t.Errorf("repository %s: %s = %q, want a code of that repository", repo, k, got[k])
			}
		}
	}
	sa, sb := state(fixtureRepo), state("repo-b")
	want(fixtureRepo, "echo from-a", "0 3 1 1 *", sa)
	want("repo-b", "echo from-b", "0 4 2 2 *", sb)
	for _, k := range []string{"job uid", "workflow uid", "job log-folder id", "workflow log-folder id"} {
		if sa[k] == sb[k] {
			t.Errorf("the two repositories share a %s: %q", k, sa[k])
		}
	}
	if n := count(`SELECT COUNT(*) FROM jobs WHERE source='git' AND name='nightly'`); n != 2 {
		t.Fatalf("git jobs named nightly = %d, want one per repository", n)
	}
	// No satellite row was left without its owner.
	for _, tbl := range []string{"definition_schedules", "reactions"} {
		if n := count(`SELECT COUNT(*) FROM ` + tbl + ` WHERE owner_source='git' AND owner_uid IS NULL`); n != 0 {
			t.Errorf("%s: %d Git rows with no owner uid", tbl, n)
		}
	}

	// Each syncs again, twice over, in turn: nothing moves. (The log-folder ids
	// are the sharpest check: by name, each sync took the other's.)
	for i := 0; i < 2; i++ {
		grBackdateAll(t, a)
		grSync(t, a, "first repository again")
		grBackdateAll(t, a)
		grSync(t, b, "second repository again")
	}
	for repo, before := range map[string]map[string]string{fixtureRepo: sa, "repo-b": sb} {
		after := state(repo)
		for k, v := range before {
			if after[k] != v {
				t.Errorf("repository %s after both synced again: %s changed from %q to %q", repo, k, v, after[k])
			}
		}
	}

	// The second repository's job is pruned and returns: a new row, the same log
	// folder, and the FIRST repository's folder is not the one it takes.
	gitRemoveFile(t, repoB, "jobs/nightly.yaml", "remove nightly")
	gitRemoveFile(t, repoB, "workflows/flow.yaml", "remove the workflow that names it")
	gitRemoveFile(t, repoB, "jobs/after.yaml", "remove the job that reacts to it")
	grBackdateAll(t, a)
	grSync(t, b, "second repository without nightly")
	if n := count(`SELECT COUNT(*) FROM jobs WHERE source='git' AND name='nightly'`); n != 1 {
		t.Fatalf("after the second repository removed its nightly, git jobs of that name = %d, want the first's", n)
	}
	if got := state(fixtureRepo); got["job schedule entries"] != "1" || got["reaction rows"] != "1" || got["job log-folder id"] != sa["job log-folder id"] {
		t.Errorf("the first repository's nightly after the second pruned its own: %v", got)
	}
	grCommitFiles(t, repoB, remoteB, files("echo from-b", "0 4 2 2 *"), "nightly returns")
	grSync(t, b, "second repository with nightly back")
	back := state("repo-b")
	if back["job uid"] == sb["job uid"] {
		t.Fatalf("the returned job has the uid of the pruned one: the prune did not happen")
	}
	if back["job log-folder id"] != sb["job log-folder id"] {
		t.Errorf("the returned job's log folder is %q, want the one it had, %q", back["job log-folder id"], sb["job log-folder id"])
	}
	if got := state(fixtureRepo)["job log-folder id"]; got != sa["job log-folder id"] {
		t.Errorf("the first repository's job's log folder changed to %q when the second's job returned", got)
	}
}

// A scope's name is unique within its agency (§0a, IN-5; Phase I2): two
// repositories of two agencies each have a scope `web`, each its own, with its
// own hosts, and neither sync touches or prunes the other's.
//
// Until I2 the name was unique across the installation: the second
// repository's file was skipped with "the scope name is already in use"
// (GR-19), and before Phase R3 it replaced the first's hosts.
func TestGR3_TwoAgenciesEachHaveTheirOwnScopeOfOneName(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "inventory/web.ini", fmt.Sprintf(grScope, "web1", "10.0.0.1"), "a's scope")
	grSync(t, a, "first repository")
	hostsOf := func(repo string) string {
		t.Helper()
		return grString(t, a.db, `SELECT COALESCE(GROUP_CONCAT(host, ','), '') FROM (SELECT h.host FROM scope_hosts h
			JOIN scopes sc ON sc.id = h.scope_id WHERE sc.name = 'web' AND COALESCE(sc.repo_id, 'global') = ? ORDER BY h.host)`, repo)
	}
	if got := hostsOf(fixtureRepo); got != "web1" {
		t.Fatalf("the first repository's scope holds %q, want web1", got)
	}
	scopeID := grString(t, a.db, `SELECT id FROM scopes WHERE name='web'`)

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"inventory/web.ini":   fmt.Sprintf(grScope, "web2", "10.0.0.2"),
		"inventory/other.ini": fmt.Sprintf(grScope, "db1", "10.0.0.9"),
	}, "b's scopes")
	grBackdateAll(t, a)
	res := grSync(t, b, "second repository")
	if rows := grProblems(t, b, "repo-b"); res.Status != "success" || grHas(rows, "inventory/web.ini") {
		t.Errorf("the sync is %q; want success, its own web written:\n%s", res.Status, strings.Join(rows, "\n"))
	}
	if res.ScopesSynced != 2 {
		t.Errorf("the sync counted %d scopes synced, want both", res.ScopesSynced)
	}
	check := func(when string) {
		t.Helper()
		if got := grString(t, a.db, `SELECT GROUP_CONCAT(r, ',') FROM (SELECT COALESCE(repo_id, 'global') || ':' || owner_agency AS r FROM scopes WHERE name = 'web' ORDER BY r)`); got != "repo-a:ag-a,repo-b:ag-b" {
			t.Errorf("%s: the scopes web are %q, want one of each repository and agency", when, got)
		}
		if got := grString(t, a.db, `SELECT id FROM scopes WHERE name = 'web' AND owner_agency = 'ag-a'`); got != scopeID {
			t.Errorf("%s: the first repository's scope is row %q, want %q untouched", when, got, scopeID)
		}
		if got := hostsOf(fixtureRepo); got != "web1" {
			t.Errorf("%s: the first repository's scope holds %q, want web1", when, got)
		}
		if got := hostsOf("repo-b"); got != "web2" {
			t.Errorf("%s: the second repository's scope holds %q, want web2", when, got)
		}
		if got := grString(t, a.db, `SELECT repo_id FROM scopes WHERE name='other'`); got != "repo-b" {
			t.Errorf("%s: the second repository's other scope has repo_id %q, want repo-b", when, got)
		}
	}
	check("after both synced")
	// And it stays so: neither repository's next sync prunes the other's.
	grBackdateAll(t, a)
	grSync(t, a, "first repository again")
	grBackdateAll(t, a)
	grSync(t, b, "second repository again")
	check("after both synced again")
}

// A scope that leaves Git while it is bound to runners and has runs waiting is
// held back from the prune WHOLE: its host membership and the records its
// hosts are reached by. Once nothing is waiting, the next sync takes all of it.
//
// Until Phase R3 (present defect 8; TestGR0_AHeldScopeLosesItsImportedHostRecords
// pinned it) the records went at the sync that held the scope: their prune
// asked neither which scope a record belonged to nor whether it was held. The
// run that was waiting was then dispatched to hosts the server no longer knew
// how to reach.
func TestGR3_AHeldScopeKeepsItsImportedHostRecords(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	grCommitFiles(t, repo, remote, map[string]string{
		"inventory/dmz.ini":   fmt.Sprintf(grScope, "web1", "10.0.0.1"),
		"inventory/plain.ini": fmt.Sprintf(grScope, "app1", "10.0.0.2") + "app2 ansible_host=10.0.0.3\n",
	}, "two scopes")
	grSync(t, svc, "first sync")
	records := func(scope string) int {
		t.Helper()
		return grCount(t, svc.db, `SELECT COUNT(*) FROM ssh_hosts h JOIN scopes sc ON sc.id = h.scope_id WHERE sc.name = ? AND h.source = 'git'`, scope)
	}
	members := func(scope string) int {
		t.Helper()
		return grCount(t, svc.db, `SELECT COUNT(*) FROM scope_hosts sh JOIN scopes sc ON sc.id = sh.scope_id WHERE sc.name = ?`, scope)
	}
	if records("dmz") != 1 || members("dmz") != 1 || records("plain") != 2 {
		t.Fatalf("the hosts were not imported: dmz %d record(s) %d member(s), plain %d record(s)", records("dmz"), members("dmz"), records("plain"))
	}

	// The operator binds dmz; a run is waiting on it.
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
	      SELECT id, 'r-dmz', 'runner-dmz-01', 'ops@example', 'now' FROM scopes WHERE name='dmz'`)
	// Of the scope's agency, ag-a: the work that holds a scope is its own agency's (I2).
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at, agencies_json)
	      VALUES ('run-waiting', 'deploy', 'bash', 'dmz', 'queued', 'seed', 'manual', 'runner', '2026-10-05T00:00:00Z', '["Agency ag-a"]')`)

	// dmz leaves Git; plain loses one host and keeps the other.
	gitRemoveFile(t, repo, "inventory/dmz.ini", "remove dmz")
	gitCommitFile(t, repo, remote, "inventory/plain.ini", fmt.Sprintf(grScope, "app1", "10.0.0.2"), "plain loses app2")
	grBackdateAll(t, svc)
	grSync(t, svc, "sync after the removal")

	if grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE name='dmz'`) != 1 || members("dmz") != 1 {
		t.Fatalf("the bound, busy scope was not held with its membership")
	}
	if n := records("dmz"); n != 1 {
		t.Errorf("the held scope has %d imported host record(s), want the one it had: it is held whole", n)
	}
	// The ordinary reap still works: a host dropped from an inventory that stays.
	if n := records("plain"); n != 1 {
		t.Errorf("the scope that stayed has %d host record(s), want 1 (app2 was removed from its inventory)", n)
	}

	// The queue drains; the next sync prunes the scope, and its records with it.
	exec(`UPDATE runs SET status='success' WHERE id='run-waiting'`)
	grBackdateAll(t, svc)
	grSync(t, svc, "sync after the queue drained")
	if grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE name='dmz'`) != 0 {
		t.Errorf("the scope was not pruned once nothing was waiting on it")
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM ssh_hosts WHERE source='git' AND hostname='web1'`); n != 0 {
		t.Errorf("the pruned scope's host record was left behind (count %d)", n)
	}
	if n := records("plain"); n != 1 {
		t.Errorf("the scope that stayed has %d host record(s) after the other was pruned, want 1", n)
	}
}

// A scope that arrives from an agency's repository is that agency's (Phase R4,
// GR-18): the operator said whose it is by connecting the repository. It is
// born so, once; a later sync neither writes nor changes it; and it comes back
// the agency's if its file leaves and returns. Each of two agencies'
// repositories' scopes is its own agency's, and stays so through every sync.
//
// Until Phase R4 every scope from Git was born Global's, whichever repository
// it came from: an agency's hosts were Global's to run on until somebody
// noticed and moved them. Until 2.5.3 the first repository here was Global's,
// its scope born Global's; the built-in agency has no repository since (CA-6),
// and the first repository is ag-a's.
func TestGR4_AScopeFromAnAgencysRepositoryIsThatAgencys(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "inventory/shared.ini", fmt.Sprintf(grScope, "g1", "10.0.0.1"), "ag-a's scope")
	grSync(t, a, "ag-a's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	gitCommitFile(t, repoB, remoteB, "inventory/theirs.ini", fmt.Sprintf(grScope, "b1", "10.0.0.2"), "the agency's scope")
	grSync(t, b, "the agency's repository")

	agencies := func(scope string) string {
		t.Helper()
		return grString(t, a.db, `SELECT COALESCE(GROUP_CONCAT(agency_id, ','), '') FROM (
			SELECT sa.agency_id FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ? ORDER BY sa.agency_id)`, scope)
	}
	if got := agencies("theirs"); got != "ag-b" {
		t.Fatalf("the scope from the agency's repository is in %q, want ag-b alone", got)
	}
	if got := agencies("shared"); got != fixtureAgency {
		t.Fatalf("the scope from ag-a's repository is in %q, want ag-a alone", got)
	}

	// The first repository's scope is held to its agency the same way (for
	// Global's repository's since 2.4.3, question 58: until then an operator
	// could assign it to an agency, and this test checked that it stayed there).
	if _, err := a.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) SELECT id, 'ag-b' FROM scopes WHERE name = 'shared'`); err == nil || !strings.Contains(err.Error(), "scope_agency_fixed") {
		t.Fatalf("assigning ag-a's repository's scope to ag-b: %v, want the scope_agency_fixed refusal", err)
	}
	for i := 0; i < 2; i++ {
		grBackdateAll(t, a)
		grSync(t, a, "ag-a's repository again")
		grBackdateAll(t, a)
		grSync(t, b, "the agency's repository again")
	}
	if got := agencies("shared"); got != fixtureAgency {
		t.Errorf("after two more syncs ag-a's repository's scope is in %q, want ag-a", got)
	}
	if got := agencies("theirs"); got != "ag-b" {
		t.Errorf("after two more syncs the agency's scope is in %q, want ag-b", got)
	}

	// The database holds it there, whoever writes: not to Global, not to another agency.
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-c', 'Agency C', 't')`); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"global", "ag-c"} {
		_, err := a.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) SELECT id, ? FROM scopes WHERE name = 'theirs'`, other)
		if err == nil || !strings.Contains(err.Error(), "scope_agency_fixed") {
			t.Errorf("giving the agency's repository's scope to %s: %v, want the scope_agency_fixed refusal", other, err)
		}
	}
	if got := agencies("theirs"); got != "ag-b" {
		t.Errorf("after the refused writes the scope is in %q", got)
	}

	// Its file leaves and returns: a new scope, and the agency's again.
	first := grString(t, a.db, `SELECT id FROM scopes WHERE name = 'theirs'`)
	gitRemoveFile(t, repoB, "inventory/theirs.ini", "remove the inventory")
	grBackdateAll(t, a)
	grSync(t, b, "the agency's repository without it")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scopes WHERE name = 'theirs'`); n != 0 {
		t.Fatalf("the scope whose file left was not pruned")
	}
	gitCommitFile(t, repoB, remoteB, "inventory/theirs.ini", fmt.Sprintf(grScope, "b1", "10.0.0.2"), "it returns")
	grSync(t, b, "the agency's repository with it back")
	if grString(t, a.db, `SELECT id FROM scopes WHERE name = 'theirs'`) == first {
		t.Fatalf("the returned scope has the id of the pruned one: the prune did not happen")
	}
	if got := agencies("theirs"); got != "ag-b" {
		t.Errorf("the scope that returned is in %q, want ag-b", got)
	}
}
