package gitlab

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A job of Global's repository that names an agency's scope (I-7, allowed
// until §0a) is not removed at Global's next sync: it is KEPT as it was, its
// row, identity and switches untouched, its file refused with the reason, and
// a notice lists it (§0a, open point 1: "each stops, and is listed"). It goes
// when its file leaves Global's repository.
//
// As first built in I1 (c9f865c) the refused file's row was pruned, and the
// job's switches and its link to its runs with it.
func TestI1_GlobalsJobOnAnAgencysScopeIsKeptAsItWas(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	grSecondRepo(t, a) // its agency owns hosts-of-repo-b
	// As an upgrade finds it: synced before §0a, switched off by an operator.
	if _, err := a.db.Exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, enabled, synced_at, repo_id, source_path)
	                        VALUES ('uid-g-theirs', 'g-theirs', 'git', 'bash', 'echo g-theirs', 'hosts-of-repo-b', 0, '2020-01-01T00:00:00Z', 'global', 'jobs/g-theirs.yaml')`); err != nil {
		t.Fatal(err)
	}
	grCommitFiles(t, repoA, remoteA, map[string]string{"jobs/g-theirs.yaml": grScopedJob("g-theirs", "hosts-of-repo-b")}, "the file is still there")
	grBackdateAll(t, a)
	res := grSync(t, a, "Global's repository after the upgrade")
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, `the scope "hosts-of-repo-b" is not one of this repository's agency's scopes`) {
		t.Errorf("Global's sync: %q (%s); want partial, the file refused", res.Status, res.ErrorMessage)
	}
	if got := grString(t, a.db, `SELECT COALESCE((SELECT uid || ' enabled=' || enabled FROM jobs WHERE source='git' AND name='g-theirs'), '(gone)')`); got != "uid-g-theirs enabled=0" {
		t.Errorf("Global's job on an agency's scope after the sync: %s; want it kept as it was (uid-g-theirs enabled=0)", got)
	}
	if err := notices.RunChecks(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM notices WHERE kind = ? AND subject = 'uid-g-theirs' AND resolved_at IS NULL AND agency_id = 'global'`,
		notices.KindGlobalJobOnAgencyScope); n != 1 {
		t.Errorf("open notices for the kept job = %d, want 1", n)
	}

	// A job of Global's own scope whose file is edited onto an agency's scope
	// is not kept: it goes, as an agency's repository's would (found by the
	// review of I1: it was kept running its old body).
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"inventory/globals.ini": "[web]\n10.0.0.9\n",
		"jobs/g-own.yaml":       grScopedJob("g-own", "globals"),
	}, "a job on Global's scope")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository with its own job")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='g-own'`); n != 1 {
		t.Fatalf("Global's job on Global's scope was not synced")
	}
	gitCommitFile(t, repoA, remoteA, "jobs/g-own.yaml", grScopedJob("g-own", "hosts-of-repo-b"), "edited onto an agency's scope")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository, the job edited")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='g-own'`); n != 0 {
		t.Errorf("Global's job edited onto an agency's scope was kept, running its old body")
	}

	// Its file leaves: it goes, and the notice clears.
	gitRemoveFile(t, repoA, "jobs/g-theirs.yaml", "the file leaves")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository without it")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND name='g-theirs'`); n != 0 {
		t.Errorf("the job stayed after its file left")
	}
	if err := notices.RunChecks(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM notices WHERE kind = ? AND resolved_at IS NULL`, notices.KindGlobalJobOnAgencyScope); n != 0 {
		t.Errorf("the notice is still open after the job went")
	}
}

// A job in an agency's repository uses its own repository's scripts and
// schedules, and watches its own repository's definitions, and nothing of
// Global's (§0a, IN-1, IN-8). A name its repository has no file for does not
// resolve, as a name that nobody has; and Global's prune of a script or a
// schedule does not wait for anybody.
//
// From Phase R4 (GR-16) until §0a such a name was looked up in Global's
// repository next, its edits reached the agency's job at Global's sync, and
// Global's prune of a row another repository used was deferred (GR-17).
func TestI1_AnAgencysJobTakesNothingFromGlobalsRepository(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-global\n",
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"jobs/nightly.yaml":     grJob("nightly", "echo nightly"),
	}, "Global's repository")
	grSync(t, a, "Global's repository")

	b, repoB, remoteB := grSecondRepo(t, a)
	const usesScript = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: uses-script\nspec:\n  script_ref: shared.sh\n  scope: hosts-of-repo-b\n"
	const usesSchedule = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: uses-schedule\nspec:\n  run_type: bash\n  command: echo hi\n  scope: hosts-of-repo-b\n  scheduleRefs:\n    - yearly\n"
	const after = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: after\nspec:\n  run_type: bash\n  command: echo after\n  scope: hosts-of-repo-b\n" +
		"  reactions:\n    - name: when-nightly\n      onKind: job\n      onName: nightly\n      onOutcome: success\n"
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"jobs/uses-script.yaml":   usesScript,
		"jobs/uses-schedule.yaml": usesSchedule,
		"jobs/after.yaml":         after,
	}, "an agency's jobs naming Global's script, schedule and job")
	res := grSync(t, b, "the agency's repository")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE repo_id='repo-b' AND name IN ('uses-script','uses-schedule','after')`); n != 0 {
		t.Errorf("%d of the agency's jobs were synced on Global's script, schedule or job", n)
	}
	for _, want := range []string{
		`script_ref "shared.sh" does not resolve to any script in scripts/`,
		`scheduleRef "yearly" does not resolve to any schedules/*.yaml`,
		`no such job "nightly"`,
	} {
		if res.Status != "partial" || !strings.Contains(res.ErrorMessage, want) {
			t.Errorf("the agency's sync is %q (%s); want partial, saying %s", res.Status, res.ErrorMessage, want)
		}
	}
	if strings.Contains(res.ErrorMessage, "installation's own") {
		t.Errorf("the agency's sync still speaks of looking in Global's repository: %s", res.ErrorMessage)
	}

	// The same names in its own repository resolve there.
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-b\n",
		"schedules/yearly.yaml": grSchedule("0 6 6 6 *"),
		"jobs/nightly.yaml":     grScopedJob("nightly", "hosts-of-repo-b"),
	}, "its own script, schedule and job of those names")
	if r := grSync(t, b, "the agency's repository with its own"); r.Status != "success" {
		t.Fatalf("the agency's sync with its own: %s (%s)", r.Status, r.ErrorMessage)
	}
	if got, want := grString(t, a.db, `SELECT COALESCE(script_uid,'') FROM jobs WHERE repo_id='repo-b' AND name='uses-script'`),
		grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id='repo-b' AND name='shared.sh'`); got != want || got == "" {
		t.Errorf("the agency's job's script is %q, want its own repository's %q", got, want)
	}

	// Global's script and schedule leave its repository: they are pruned at
	// Global's sync, and no notice is opened for anybody.
	gitRemoveFile(t, repoA, "scripts/shared.sh", "Global's script leaves")
	gitRemoveFile(t, repoA, "schedules/yearly.yaml", "and its schedule")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository without them")
	if n := grCount(t, a.db, `SELECT (SELECT COUNT(*) FROM scripts WHERE repo_id='global' AND name='shared.sh')
	                          + (SELECT COUNT(*) FROM schedules WHERE repo_id='global' AND name='yearly')`); n != 0 {
		t.Errorf("Global's script or schedule was kept after its file left (%d rows)", n)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM notices WHERE kind = 'prune_deferred'`); n != 0 {
		t.Errorf("%d deferred-prune notices were opened", n)
	}
}

// A Git schedule's edit reaches the in-app definitions of its repository's
// agency only (§0a; the owner's answer to question 32): an agency's in-app
// job bound to Global's schedule before §0a keeps the timing it copied, and
// Global's own in-app job follows. Until §0a Global's repository's schedules
// retimed every in-app definition bound to them.
func TestI1_GlobalsScheduleEditReachesGlobalsDefinitionsOnly(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{"schedules/yearly.yaml": grSchedule("0 3 1 1 *")}, "Global's schedule")
	grSync(t, a, "Global's repository")
	grSecondRepo(t, a) // its agency owns hosts-of-repo-b
	for _, j := range [][2]string{{"j-b", "hosts-of-repo-b"}, {"j-g", ""}} {
		var scope any
		if j[1] != "" {
			scope = j[1]
		}
		if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, scope) VALUES(?, ?, 'cronomicon', 'bash', 't', ?)`, j[0], "app-"+j[0], scope); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, schedule_uid, source_ref)
		                        SELECT 'cronomicon', 'job', ?, 'yearly', '0 3 1 1 *', 1, ?, uid, 'yearly' FROM schedules WHERE repo_id = 'global' AND name = 'yearly'`,
			"app-"+j[0], j[0]); err != nil {
			t.Fatal(err)
		}
	}
	gitCommitFile(t, repoA, remoteA, "schedules/yearly.yaml", grSchedule("0 4 4 4 *"), "Global edits its schedule")
	grSync(t, a, "Global's repository, edited")
	cronOf := func(uid string) string {
		t.Helper()
		return grString(t, a.db, `SELECT cron FROM definition_schedules WHERE owner_uid = ? AND name = 'yearly'`, uid)
	}
	if got := cronOf("j-g"); got != "0 4 4 4 *" {
		t.Errorf("Global's in-app job fires at %q after Global's edit, want the new timing", got)
	}
	if got := cronOf("j-b"); got != "0 3 1 1 *" {
		t.Errorf("the agency's in-app job fires at %q after Global's edit; want the timing it copied", got)
	}
}

// An agency's file names its own agency's calendars (§0a; I2: a calendar's
// name is unique within an agency, and the upgrade gave each agency a copy of
// what its files named of Global's). A name only Global has is unknown to the
// agency, and the file is refused as it is for any unknown calendar; the same
// name among the agency's own is found. (Between I1 and I2 Global's was
// synced with a warning, the binding live until the copies.)
func TestI2_AnAgencysFileNamesItsOwnCalendars(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	if _, err := a.db.Exec(`INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 0, 0, 't', 't', 'global')`); err != nil {
		t.Fatal(err)
	}
	b, repoB, remoteB := grSecondRepo(t, a)
	const job = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: on-holidays\nspec:\n  run_type: bash\n  command: echo hi\n  scope: hosts-of-repo-b\n" +
		"  schedules:\n    - name: nightly\n      cron: \"0 2 * * *\"\n      skipCalendars: [holidays]\n"
	gitCommitFile(t, repoB, remoteB, "jobs/on-holidays.yaml", job, "a job naming a calendar only Global has")
	res := grSync(t, b, "the agency's repository")
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, "unknown calendar") {
		t.Errorf("the agency's sync: %s (%s); want partial, the calendar unknown to the agency", res.Status, res.ErrorMessage)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE repo_id = 'repo-b' AND name = 'on-holidays'`); n != 0 {
		t.Errorf("the job naming a calendar only Global has was synced")
	}
	// The agency's own calendar of the name: found.
	if _, err := a.db.Exec(`INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 0, 0, 't', 't', 'ag-b')`); err != nil {
		t.Fatal(err)
	}
	grBackdateAll(t, a)
	res = grSync(t, b, "the agency's repository, with its own calendar")
	if res.Status != "success" {
		t.Errorf("the agency's sync with its own holidays: %s (%s); want success", res.Status, res.ErrorMessage)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE repo_id = 'repo-b' AND name = 'on-holidays'`); n != 1 {
		t.Errorf("the job naming the agency's own calendar was not synced")
	}
}

// Global's own repository takes nothing from anywhere: a name it has no file
// for does not resolve, as before.
func TestGR4_GlobalsRepositoryBorrowsNothing(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/theirs.sh":     "#!/bin/bash\necho theirs\n",
		"schedules/yearly.yaml": grSchedule("0 5 5 5 *"),
	}, "an agency's script and schedule")
	grSync(t, b, "the agency's repository")
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"jobs/wants-script.yaml":   "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: wants-script\nspec:\n  script_ref: theirs.sh\n",
		"jobs/wants-schedule.yaml": "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: wants-schedule\nspec:\n  run_type: bash\n  command: echo hi\n  scheduleRefs:\n    - yearly\n",
	}, "Global's jobs naming an agency's script and schedule")
	res := grSync(t, a, "Global's repository")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE name IN ('wants-script','wants-schedule')`); n != 0 {
		t.Errorf("Global's jobs were synced on an agency's script or schedule (count %d)", n)
	}
	if res.Status != "partial" || strings.Contains(res.ErrorMessage, "installation's own") {
		t.Errorf("Global's sync: %q (%s)", res.Status, res.ErrorMessage)
	}

	// Nor from its own past: a script whose file has left Global's repository
	// is not found again in the tables, where its row still is until the prune.
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/old.sh":     "#!/bin/bash\necho old\n",
		"jobs/uses-old.yaml": "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: uses-old\nspec:\n  script_ref: old.sh\n",
	}, "a script and a job that uses it")
	gitRemoveFile(t, repoA, "jobs/wants-script.yaml", "remove a job that does not resolve")
	gitRemoveFile(t, repoA, "jobs/wants-schedule.yaml", "remove the other")
	if r := grSync(t, a, "Global's repository with the script"); r.Status != "success" {
		t.Fatalf("Global's sync with the script and its job: %s (%s)", r.Status, r.ErrorMessage)
	}
	gitRemoveFile(t, repoA, "scripts/old.sh", "the script's file leaves; the job still names it")
	res = grSync(t, a, "Global's repository without the script")
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, `script_ref "old.sh" does not resolve`) {
		t.Errorf("Global's sync after its script's file left: %q (%s); want partial, the job's script not resolving", res.Status, res.ErrorMessage)
	}
}

// An in-app job that has lost its script is joined to a returning script of
// that name in ITS agency's repository, and in no other (§0a; the owner's yes
// to question 31). An agency's repository joins only its own agency's in-app
// jobs, and Global's only Global's (a job with no scope).
//
// Until Phase R4 only Global's repository joined anything, and joined every
// in-app job of the name; from R4 until §0a Global's joined the other
// agencies' jobs too, when their own repository had no script of the name.
func TestI1_AnInAppJobIsRejoinedInItsAgencysRepositoryOnly(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/task.sh":        "#!/bin/bash\necho task-of-global\n",
		"scripts/only-global.sh": "#!/bin/bash\necho only-global\n",
	}, "Global's scripts")
	b, repoB, remoteB := grSecondRepo(t, a) // its agency ag-b owns the scope hosts-of-repo-b
	gitCommitFile(t, repoB, remoteB, "scripts/task.sh", "#!/bin/bash\necho task-of-b\n", "the agency's script of the same name")
	for _, j := range [][3]string{
		{"j-global", "", "task.sh"},
		{"j-b", "hosts-of-repo-b", "task.sh"},
		{"j-b-other", "hosts-of-repo-b", "only-global.sh"},
	} {
		var scope any
		if j[1] != "" {
			scope = j[1]
		}
		if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, scope) VALUES(?, ?, 'cronomicon', 'bash', 't', ?, ?)`,
			j[0], "app-"+j[0], j[2], scope); err != nil {
			t.Fatal(err)
		}
	}
	joined := func(job string) string {
		t.Helper()
		return grString(t, a.db, `SELECT COALESCE((SELECT s.repo_id || '/' || s.name FROM scripts s WHERE s.uid = j.script_uid), '(none)') FROM jobs j WHERE j.uid = ?`, job)
	}

	grSync(t, b, "the agency's repository")
	for job, want := range map[string]string{"j-global": "(none)", "j-b": "repo-b/task.sh", "j-b-other": "(none)"} {
		if got := joined(job); got != want {
			t.Errorf("after the agency's sync %s is joined to %s, want %s", job, got, want)
		}
	}

	// The agency's job loses its script again (as a prune of it would leave it),
	// and Global's repository syncs.
	if _, err := a.db.Exec(`UPDATE jobs SET script_uid = NULL WHERE uid = 'j-b'`); err != nil {
		t.Fatal(err)
	}
	grSync(t, a, "Global's repository")
	for job, want := range map[string]string{
		"j-global":  "global/task.sh",
		"j-b":       "(none)", // the agency's: its own repository's to join
		"j-b-other": "(none)", // the agency's: Global's script is not its (§0a; until then joined)
	} {
		if got := joined(job); got != want {
			t.Errorf("after Global's sync %s is joined to %s, want %s", job, got, want)
		}
	}
	// A join is said by the sync that makes it (2.4.0, with the owner's
	// decision of 2026-10-13). Global's sync joined Global's job alone.
	global := grProblems(t, a, "global")
	if !grHas(global, "warning", "had no script and is joined", "app-j-global") {
		t.Errorf("Global's sync does not say that it joined app-j-global to its script:\n%s", strings.Join(global, "\n"))
	}
	said := 0
	for _, line := range global {
		if strings.Contains(line, "had no script and is joined") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("Global's sync says %d joins, want the one it made:\n%s", said, strings.Join(global, "\n"))
	}

	// And a job that has a script is not moved to another.
	grSync(t, b, "the agency's repository again")
	if got := joined("j-global"); got != "global/task.sh" {
		t.Errorf("the agency's sync moved Global's in-app job to %s", got)
	}
	if got := joined("j-b"); got != "repo-b/task.sh" {
		t.Errorf("the agency's job is joined to %s after its repository synced, want its own", got)
	}
}
