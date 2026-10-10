package gitlab

import (
	"strings"
	"testing"
)

// A job in an agency's repository uses the installation's own script and
// schedule when its repository has none of that name, and never another
// agency's (Phase R4, GR-16); a reaction there may watch one of Global's
// definitions. What Global's repository changes reaches the job at Global's
// sync. The repository's own, once it has one, comes first; and a name it has
// a BROKEN file for is not quietly taken from Global's instead.
//
// Until Phase R4 a name was looked up in the job's own repository and nowhere
// else: an agency's job could not use a script Global supplies to everyone.
func TestGR4_AJobUsesGlobalsScriptAndScheduleNotAnotherAgencys(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	const wrapped = "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: wrapped\nspec:\n  run_type: bash\n  command: echo wrapped-by-global\n"
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-global\n",
		"scripts/wrapped.yaml":  wrapped,
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"jobs/nightly.yaml":     grJob("nightly", "echo nightly"),
	}, "Global's repository")
	grSync(t, a, "Global's repository")

	c, repoC, remoteC := grRepo(t, a, "repo-c", "ag-c")
	grCommitFiles(t, repoC, remoteC, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-c\n",
		"schedules/yearly.yaml": grSchedule("0 5 5 5 *"),
	}, "another agency's repository, with the same names")
	grSync(t, c, "another agency's repository")

	b, repoB, remoteB := grSecondRepo(t, a)
	const uses = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: uses\nspec:\n  script_ref: shared.sh\n  scheduleRefs:\n    - yearly\n"
	const after = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: after\nspec:\n  run_type: bash\n  command: echo after\n" +
		"  reactions:\n    - name: when-nightly\n      onKind: job\n      onName: nightly\n      onOutcome: success\n"
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/uses.yaml": uses, "jobs/after.yaml": after}, "the agency's repository")
	if r := grSync(t, b, "the agency's repository"); r.Status != "success" {
		t.Fatalf("the sync of a repository that uses Global's script, schedule and job: %s (%s)", r.Status, r.ErrorMessage)
	}

	str := func(q string, args ...any) string { t.Helper(); return grString(t, a.db, q, args...) }
	script := func(repo, name string) string {
		t.Helper()
		return str(`SELECT uid FROM scripts WHERE repo_id = ? AND name = ?`, repo, name)
	}
	sched := func(repo string) string {
		t.Helper()
		return str(`SELECT uid FROM schedules WHERE source='git' AND repo_id = ? AND name='yearly'`, repo)
	}
	job := func(col string) string {
		t.Helper()
		return str(`SELECT COALESCE(` + col + `, '') FROM jobs WHERE source='git' AND repo_id='repo-b' AND name='uses'`)
	}
	entry := func(col string) string {
		t.Helper()
		return str(`SELECT COALESCE(d.` + col + `, '') FROM definition_schedules d JOIN jobs j ON j.uid = d.owner_uid
		             WHERE j.repo_id='repo-b' AND j.name='uses'`)
	}
	hashOf := func(repo, name string) string {
		t.Helper()
		return str(`SELECT content_hash FROM scripts WHERE repo_id = ? AND name = ?`, repo, name)
	}
	if got := job("script_uid"); got != script("global", "shared.sh") || got == script("repo-c", "shared.sh") || got == "" {
		t.Fatalf("the job's script is %q; want Global's %q, not the other agency's %q", got, script("global", "shared.sh"), script("repo-c", "shared.sh"))
	}
	if got := job("content_hash"); got != hashOf("global", "shared.sh") {
		t.Errorf("the job's body is not Global's script's (hash %q)", got)
	}
	if got := entry("schedule_uid"); got != sched("global") || got == "" {
		t.Errorf("the job's schedule is %q; want Global's %q", got, sched("global"))
	}
	if got := entry("cron"); got != "0 3 1 1 *" {
		t.Errorf("the job fires at %q; want Global's schedule's timing, not the other agency's", got)
	}
	if got, want := str(`SELECT COALESCE(r.on_uid,'') FROM reactions r JOIN jobs j ON j.uid = r.owner_uid WHERE j.repo_id='repo-b' AND j.name='after'`),
		str(`SELECT uid FROM jobs WHERE source='git' AND repo_id='global' AND name='nightly'`); got != want || got == "" {
		t.Errorf("the reaction watches %q, want Global's nightly %q", got, want)
	}
	// It did not make them the agency's: no row of its own for either.
	if n := grCount(t, a.db, `SELECT (SELECT COUNT(*) FROM scripts WHERE repo_id='repo-b') + (SELECT COUNT(*) FROM schedules WHERE repo_id='repo-b')`); n != 0 {
		t.Errorf("the agency's repository has %d scripts and schedules of its own; it has no file for any", n)
	}

	// Global's repository edits both. Its sync carries them to the job.
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-global, edited\n",
		"schedules/yearly.yaml": grSchedule("0 4 4 4 *"),
	}, "Global edits what the agency's job uses")
	before := job("content_hash")
	grSync(t, a, "Global's repository, edited")
	if got := job("content_hash"); got == before || got != hashOf("global", "shared.sh") {
		t.Errorf("after Global's script was edited the agency's job's body is %q; want Global's new one %q", got, hashOf("global", "shared.sh"))
	}
	if got := entry("cron"); got != "0 4 4 4 *" {
		t.Errorf("after Global's schedule was edited the agency's job fires at %q", got)
	}
	if got := job("schedule"); got != "0 4 4 4 *" {
		t.Errorf("the job's displayed schedule is %q after Global's edit", got)
	}

	// The repository gets its own of both names: its own comes first.
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-b\n",
		"schedules/yearly.yaml": grSchedule("0 6 6 6 *"),
	}, "its own script and schedule of those names")
	grSync(t, b, "the agency's repository with its own")
	if got := job("script_uid"); got != script("repo-b", "shared.sh") || got == "" {
		t.Errorf("with a script of its own the job's script is %q, want its repository's %q", got, script("repo-b", "shared.sh"))
	}
	if got := entry("schedule_uid") + " " + entry("cron"); got != sched("repo-b")+" 0 6 6 6 *" {
		t.Errorf("with a schedule of its own the job's entry is %q", got)
	}
	// And Global's next edit is no longer its concern.
	gitCommitFile(t, repoA, remoteA, "scripts/shared.sh", "#!/bin/bash\necho from-global, again\n", "Global edits its script again")
	grSync(t, a, "Global's repository, edited again")
	if got := job("content_hash"); got != hashOf("repo-b", "shared.sh") {
		t.Errorf("Global's edit changed the body of a job that uses its own repository's script")
	}

	// A name the repository has a BROKEN file for is not taken from Global's.
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/wrapped.yaml": "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: wrapped\nspec:\n  run_type: bash\n  scriptPath: scripts/not-there.sh\n",
		"jobs/wraps.yaml":      "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: wraps\nspec:\n  script_ref: wrapped\n",
	}, "a script of Global's name that does not validate, and a job that names it")
	res := grSync(t, b, "the agency's repository with a broken script")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='repo-b' AND name='wraps'`); n != 0 {
		t.Errorf("a job whose own repository's script is broken was synced on Global's script of that name")
	}
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, `script_ref "wrapped" does not resolve`) {
		t.Errorf("the sync is %q (%s); want partial, saying the job's script does not resolve", res.Status, res.ErrorMessage)
	}

	// Its own files leave: the job is Global's script's and schedule's again.
	for _, f := range []string{"scripts/shared.sh", "schedules/yearly.yaml", "scripts/wrapped.yaml", "jobs/wraps.yaml"} {
		gitRemoveFile(t, repoB, f, "remove "+f)
	}
	grBackdateAll(t, a)
	if r := grSync(t, b, "the agency's repository without its own"); r.Status != "success" {
		t.Fatalf("the sync after its own files left: %s (%s)", r.Status, r.ErrorMessage)
	}
	if got := job("script_uid"); got != script("global", "shared.sh") {
		t.Errorf("after its own script left, the job's script is %q, want Global's", got)
	}
	if got := entry("schedule_uid"); got != sched("global") {
		t.Errorf("after its own schedule left, the job's schedule is %q, want Global's", got)
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
// that name in ITS agency's repository, then in Global's (Phase R4, GR-16).
// An agency's repository joins only its own agency's in-app jobs; Global's
// joins the others, and leaves a job whose own agency's repository has a
// script of the name to that repository.
//
// Until Phase R4 only Global's repository joined anything, and joined every
// in-app job of the name.
func TestGR4_AnInAppJobIsRejoinedInItsAgencysRepositoryThenGlobals(t *testing.T) {
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
		"j-b":       "(none)", // its own agency's repository has a task.sh: that one's to join
		"j-b-other": "global/only-global.sh",
	} {
		if got := joined(job); got != want {
			t.Errorf("after Global's sync %s is joined to %s, want %s", job, got, want)
		}
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
