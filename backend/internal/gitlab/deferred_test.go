package gitlab

import (
	"strings"
	"testing"
)

// grDeferred reads the open deferred-prune notices: subject → "agency|detail".
func grDeferred(t *testing.T, svc *Service) map[string]string {
	t.Helper()
	rows, err := svc.db.Query(`SELECT subject, agency_id, detail FROM notices WHERE kind = 'prune_deferred' AND resolved_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var subject, agency, detail string
		if err := rows.Scan(&subject, &agency, &detail); err != nil {
			t.Fatal(err)
		}
		out[subject] = agency + "|" + detail
	}
	return out
}

// Global's sync does not prune a script or a schedule that another
// repository's definition still uses (Phase R4, GR-17). The row is kept as it
// was when its file left; each agency that uses it is told, with its own
// definitions and nobody else's; and the row goes at the first sync of
// Global's repository at which nothing of another repository's uses it.
//
// Before GR-16 nothing of another repository's could use Global's rows. With
// it, an ordinary clean-up of Global's repository would have taken the body
// from under an agency's job, and that agency's next sync would have dropped
// the job for a name that resolves to nothing.
func TestGR4_GlobalsSyncDoesNotPruneWhatAnotherRepositoryUses(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-global\n",
		"scripts/unused.sh":     "#!/bin/bash\necho nobody uses this\n",
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"schedules/idle.yaml":   strings.Replace(grSchedule("0 4 4 4 *"), "name: yearly", "name: idle", 1),
	}, "Global's repository")
	grSync(t, a, "Global's repository")

	const uses = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: %s\nspec:\n  script_ref: shared.sh\n  scheduleRefs:\n    - yearly\n"
	const flow = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  scheduleRefs:\n    - yearly\n  steps:\n    - name: s1\n      job: uses-b\n"
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"jobs/uses-b.yaml":    strings.Replace(uses, "%s", "uses-b", 1),
		"workflows/flow.yaml": flow,
	}, "an agency's job and workflow that use Global's")
	grSync(t, b, "the agency's repository")
	c, repoC, remoteC := grRepo(t, a, "repo-c", "ag-c")
	gitCommitFile(t, repoC, remoteC, "jobs/uses-c.yaml", strings.Replace(uses, "%s", "uses-c", 1), "another agency's job that uses the script")
	grSync(t, c, "another agency's repository")

	count := func(q string, args ...any) int { t.Helper(); return grCount(t, a.db, q, args...) }
	script := grString(t, a.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='shared.sh'`)
	sched := grString(t, a.db, `SELECT uid FROM schedules WHERE repo_id='global' AND name='yearly'`)
	body := grString(t, a.db, `SELECT content_hash FROM scripts WHERE uid = ?`, script)

	// All four files leave Global's repository.
	for _, f := range []string{"scripts/shared.sh", "scripts/unused.sh", "schedules/yearly.yaml", "schedules/idle.yaml"} {
		gitRemoveFile(t, repoA, f, "remove "+f)
	}
	grBackdateAll(t, a)
	if r := grSync(t, a, "Global's repository, cleaned up"); r.Status != "success" {
		t.Fatalf("Global's sync after the clean-up: %s (%s)", r.Status, r.ErrorMessage)
	}
	if n := count(`SELECT COUNT(*) FROM scripts WHERE uid = ? AND content_hash = ?`, script, body); n != 1 {
		t.Fatalf("the script two agencies' jobs use was pruned, or changed")
	}
	if n := count(`SELECT COUNT(*) FROM schedules WHERE uid = ? AND cron = '0 3 1 1 *'`, sched); n != 1 {
		t.Fatalf("the schedule two agencies' definitions use was pruned, or changed")
	}
	if n := count(`SELECT (SELECT COUNT(*) FROM scripts WHERE name='unused.sh') + (SELECT COUNT(*) FROM schedules WHERE name='idle')`); n != 0 {
		t.Errorf("what nobody uses was not pruned (%d rows left)", n)
	}
	// The jobs are joined as they were.
	if n := count(`SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id IN ('repo-b','repo-c') AND script_uid = ?`, script); n != 2 {
		t.Errorf("jobs still joined to the kept script = %d, want both agencies'", n)
	}

	got := grDeferred(t, a)
	for subject, want := range map[string][]string{
		"script:" + script + ":repo-b":  {"ag-b|", "The script shared.sh", "uses-b"},
		"script:" + script + ":repo-c":  {"ag-c|", "The script shared.sh", "uses-c"},
		"schedule:" + sched + ":repo-b": {"ag-b|", "The schedule yearly", "job uses-b", "workflow flow"},
		"schedule:" + sched + ":repo-c": {"ag-c|", "The schedule yearly", "job uses-c"},
	} {
		n, ok := got[subject]
		if !ok {
			t.Errorf("no notice %s; open: %v", subject, got)
			continue
		}
		for _, w := range want {
			if !strings.Contains(n, w) {
				t.Errorf("the notice %s does not say %q: %s", subject, w, n)
			}
		}
	}
	if len(got) != 4 {
		t.Errorf("open notices = %d, want one per kept row per agency that uses it: %v", len(got), got)
	}
	// Each names its own repository's definitions and nobody else's.
	for subject, n := range got {
		other := "uses-c"
		if strings.HasSuffix(subject, ":repo-c") {
			other = "uses-b"
		}
		if strings.Contains(n, other) || strings.Contains(n, "flow") && strings.HasSuffix(subject, ":repo-c") {
			t.Errorf("the notice %s names another agency's definition: %s", subject, n)
		}
	}

	// The agencies' repositories go on syncing, on the kept rows.
	grBackdateAll(t, a)
	if r := grSync(t, b, "the agency's repository after the clean-up"); r.Status != "success" {
		t.Fatalf("the agency's sync on a kept script and schedule: %s (%s)", r.Status, r.ErrorMessage)
	}
	if n := count(`SELECT COUNT(*) FROM jobs WHERE repo_id='repo-b' AND name='uses-b' AND script_uid = ?`, script); n != 1 {
		t.Errorf("the agency's job is no longer joined to the kept script after its own sync")
	}
	// A second sync of Global's keeps them still, and opens nothing twice.
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository again")
	if n := count(`SELECT (SELECT COUNT(*) FROM scripts WHERE uid = ?) + (SELECT COUNT(*) FROM schedules WHERE uid = ?)`, script, sched); n != 2 {
		t.Fatalf("a second sync of Global's pruned what is still used")
	}
	if got := grDeferred(t, a); len(got) != 4 {
		t.Errorf("open notices after a second sync of Global's = %d, want the same four", len(got))
	}

	// One agency does what its notice asks: a script and a schedule of its own.
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"scripts/shared.sh":     "#!/bin/bash\necho from-b\n",
		"schedules/yearly.yaml": grSchedule("0 6 6 6 *"),
	}, "the agency's own script and schedule")
	grSync(t, b, "the agency's repository with its own")
	got = grDeferred(t, a)
	if len(got) != 2 {
		t.Errorf("after one agency stopped using them, open notices = %d (%v); want the other agency's two", len(got), got)
	}
	for subject := range got {
		if !strings.HasSuffix(subject, ":repo-c") {
			t.Errorf("the notice %s is still open for the agency that stopped using the row", subject)
		}
	}
	// Still used by the other: still kept.
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository, one agency still using them")
	if n := count(`SELECT (SELECT COUNT(*) FROM scripts WHERE uid = ?) + (SELECT COUNT(*) FROM schedules WHERE uid = ?)`, script, sched); n != 2 {
		t.Fatalf("the rows were pruned while one agency still uses them")
	}

	// The other agency's job leaves. Nothing uses them: Global's next sync
	// takes them, and nothing is left in the inbox.
	gitRemoveFile(t, repoC, "jobs/uses-c.yaml", "the job leaves")
	grBackdateAll(t, a)
	grSync(t, c, "the other agency's repository without the job")
	if got := grDeferred(t, a); len(got) != 0 {
		t.Errorf("open notices after nothing uses the rows = %v", got)
	}
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository, nothing using them")
	if n := count(`SELECT (SELECT COUNT(*) FROM scripts WHERE uid = ?) + (SELECT COUNT(*) FROM schedules WHERE uid = ?)`, script, sched); n != 0 {
		t.Errorf("the rows nobody uses any more were not pruned (%d left)", n)
	}
}

// What a definition built in the app uses is not held back: that is older
// than this rule and is its own (the job keeps its copy of the script; the
// schedule's users go on firing and the inbox says so).
func TestGR4_AnInAppUserDoesNotDeferAPrune(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	gitCommitFile(t, repoA, remoteA, "scripts/shared.sh", "#!/bin/bash\necho from-global\n", "a script")
	grSync(t, a, "Global's repository")
	script := grString(t, a.db, `SELECT uid FROM scripts WHERE name='shared.sh'`)
	if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid) VALUES('j-app','app-job','cronomicon','bash','t','shared.sh',?)`, script); err != nil {
		t.Fatal(err)
	}
	gitRemoveFile(t, repoA, "scripts/shared.sh", "the script leaves")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository without it")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scripts WHERE uid = ?`, script); n != 0 {
		t.Errorf("a script only an in-app job uses was kept")
	}
	if got := grDeferred(t, a); len(got) != 0 {
		t.Errorf("a deferred-prune notice for an in-app user: %v", got)
	}
}
