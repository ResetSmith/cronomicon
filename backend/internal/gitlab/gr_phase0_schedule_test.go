package gitlab

import (
	"context"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"strings"
	"testing"
)

const grYearlyGitJob = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: git-job\nspec:\n  run_type: bash\n  command: echo hi\n  scheduleRefs:\n    - yearly\n"

// An in-app definition follows its Git schedule.
//
// A definition bound to a reusable schedule holds a COPY of it in
// definition_schedules. Sync writes that copy afresh for a Git job or workflow
// on every sync, and the in-app edit of an in-app schedule propagates to its
// users. Until this test was inverted
// (TestGR0_AGitSchedulesEditDoesNotReachInAppDefinitions pinned the opposite;
// present defect 13) nothing wrote it for a definition built in the app and
// bound to a GIT schedule, which went on firing at the old time after the
// schedule was changed in Git. Sync now carries the edit to every in-app entry
// tied to the schedule by its uid, and to nothing else.
func TestAnInAppDefinitionFollowsItsGitSchedule(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	grCommitFiles(t, repo, remote, map[string]string{
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"jobs/git-job.yaml":     grYearlyGitJob,
	}, "a schedule and a Git job bound to it")
	grSync(t, svc, "first sync")
	sched := grString(t, svc.db, `SELECT uid FROM schedules WHERE source='git' AND name='yearly'`)
	first := grString(t, svc.db, `SELECT cron FROM definition_schedules WHERE owner_source='git' AND owner_name='git-job'`)
	entry := func(ownerUID, name string) string {
		t.Helper()
		return grString(t, svc.db, `SELECT cron FROM definition_schedules WHERE owner_uid=? AND name=?`, ownerUID, name)
	}
	bind := func(kind, ownerUID, owner, name string, position int, ref, schedUID any) {
		t.Helper()
		exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
		      VALUES('cronomicon', ?, ?, ?, ?, ?, ?, ?, ?)`, kind, owner, name, first, position, ref, ownerUID, schedUID)
	}

	// An in-app job bound to the Git schedule, as the composer writes it: a copy
	// of the schedule's timing, its name, and its uid. Its display column
	// mirrors its first entry.
	exec(`INSERT INTO jobs(uid, name, source, run_type, command, schedule, synced_at) VALUES('j-app', 'app-job', 'cronomicon', 'bash', 'echo hi', ?, 't')`, first)
	bind("job", "j-app", "app-job", "yearly", 0, "yearly", sched)
	// An inline entry of the same job: its own timing, no schedule.
	bind("job", "j-app", "app-job", "own", 1, nil, nil)
	// An in-app workflow bound to it.
	exec(`INSERT INTO workflows(uid, name, source, steps, schedule, synced_at) VALUES('w-app', 'app-flow', 'cronomicon', '[]', ?, 't')`, first)
	bind("workflow", "w-app", "app-flow", "yearly", 0, "yearly", sched)
	// A job in the recycle bin bound to it: restored, it fires at once.
	exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at, deleted_at) VALUES('j-bin', 'binned-job', 'cronomicon', 'bash', 'echo hi', 't', '2026-01-01T00:00:00Z')`)
	bind("job", "j-bin", "binned-job", "yearly", 0, "yearly", sched)
	// What must NOT follow: an entry of that name tied to ANOTHER schedule (an
	// in-app `yearly`), and one tied to none (the ambiguous entry of 1300).
	exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, owner_agency) VALUES('s-app', 'yearly', 'cronomicon', ?, 'h', 'global')`, first)
	exec(`INSERT INTO jobs(uid, name, source, run_type, command, schedule, synced_at) VALUES('j-other', 'other-job', 'cronomicon', 'bash', 'echo hi', ?, 't')`, first)
	bind("job", "j-other", "other-job", "yearly", 0, "yearly", "s-app")
	exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES('j-untied', 'untied-job', 'cronomicon', 'bash', 'echo hi', 't')`)
	bind("job", "j-untied", "untied-job", "yearly", 0, "yearly", nil)

	// The schedule is edited in Git.
	gitCommitFile(t, repo, remote, "schedules/yearly.yaml", grSchedule("0 4 2 2 *"), "edit the schedule")
	grSync(t, svc, "sync after the edit")
	second := grString(t, svc.db, `SELECT cron FROM definition_schedules WHERE owner_source='git' AND owner_name='git-job'`)
	if second == first {
		t.Fatalf("the Git job's entry did not follow the edit (%q)", second)
	}
	for _, c := range []struct{ uid, what string }{
		{"j-app", "the in-app job"}, {"w-app", "the in-app workflow"}, {"j-bin", "the in-app job in the recycle bin"},
	} {
		if got := entry(c.uid, "yearly"); got != second {
			t.Errorf("%s still fires at %q after its Git schedule was edited, want %q", c.what, got, second)
		}
	}
	if got := grString(t, svc.db, `SELECT COALESCE(schedule,'') FROM jobs WHERE uid='j-app'`); got != second {
		t.Errorf("the in-app job's schedule column = %q, want its first entry's new timing %q", got, second)
	}
	if got := grString(t, svc.db, `SELECT COALESCE(schedule,'') FROM workflows WHERE uid='w-app'`); got != second {
		t.Errorf("the in-app workflow's schedule column = %q, want %q", got, second)
	}
	if got := entry("j-app", "own"); got != first {
		t.Errorf("the job's inline entry was rewritten to %q", got)
	}
	if got := entry("j-other", "yearly"); got != first {
		t.Errorf("an entry tied to the in-app schedule of that name followed Git's edit: %q", got)
	}
	if got := grString(t, svc.db, `SELECT COALESCE(schedule,'') FROM jobs WHERE uid='j-other'`); got != first {
		t.Errorf("the schedule column of a job bound to another schedule was rewritten: %q", got)
	}
	if got := entry("j-untied", "yearly"); got != first {
		t.Errorf("an entry tied to no schedule followed Git's edit: %q", got)
	}
	// Nothing else about the entries moved.
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM definition_schedules WHERE owner_source='cronomicon'`); n != 6 {
		t.Errorf("in-app entries after the sync = %d, want the six there were", n)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM definition_schedules WHERE owner_source='cronomicon' AND schedule_uid=?`, sched); n != 3 {
		t.Errorf("in-app entries tied to the Git schedule = %d, want 3", n)
	}
}

// The other half of present defect 13, as the owner decided it (2026-10-09):
// when a schedule leaves Git, the in-app definitions bound to it go on firing
// at the timing they have, holding the uid of a schedule that is gone, AND THE
// INBOX SAYS SO (Phase R4), naming each and what to do. The notice clears when
// the definition is bound to another schedule or given a timing of its own.
//
// Until Phase R4 (TestGR0_AnInAppDefinitionOutlivesItsPrunedGitSchedule pinned
// it) nothing said so: the entry named a schedule that no list shows, at a
// timing no edit would ever change again.
func TestGR4_AnInAppDefinitionOutlivesItsPrunedGitScheduleAndTheInboxSaysSo(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	grCommitFiles(t, repo, remote, map[string]string{
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"jobs/git-job.yaml":     grYearlyGitJob,
	}, "a schedule and a Git job bound to it")
	grSync(t, svc, "first sync")
	sched := grString(t, svc.db, `SELECT uid FROM schedules WHERE source='git' AND name='yearly'`)
	first := grString(t, svc.db, `SELECT cron FROM definition_schedules WHERE owner_source='git' AND owner_name='git-job'`)
	if _, err := svc.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES('j-app', 'app-job', 'cronomicon', 'bash', 'echo hi', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	      VALUES('cronomicon', 'job', 'app-job', 'yearly', ?, 0, 'yearly', 'j-app', ?)`, first, sched); err != nil {
		t.Fatal(err)
	}

	// The schedule leaves Git (and the Git job with it, or the sync would
	// refuse the dangling reference and prune nothing).
	gitRemoveFile(t, repo, "schedules/yearly.yaml", "remove the schedule")
	gitRemoveFile(t, repo, "jobs/git-job.yaml", "remove the job")
	grBackdate(t, svc.db)
	grSync(t, svc, "sync after the removal")
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM schedules WHERE name='yearly'`); n != 0 {
		t.Fatalf("the schedule was not pruned (count %d)", n)
	}
	switch n := grCount(t, svc.db, `SELECT COUNT(*) FROM definition_schedules WHERE owner_uid='j-app'`); n {
	case 1:
		// Today: the in-app job still fires, bound to a schedule that is gone.
		if got := grString(t, svc.db, `SELECT cron || ' ' || source_ref || ' ' || schedule_uid FROM definition_schedules WHERE owner_uid='j-app'`); got != first+" yearly "+sched {
			t.Errorf("the orphaned entry is %q, want it as it was: %q", got, first+" yearly "+sched)
		}
	case 0:
		t.Errorf("the in-app job was detached when its schedule left Git: the decision was that it goes on firing")
	default:
		t.Errorf("the in-app job has %d entries after its schedule was pruned", n)
	}

	gone := func() (agency, detail string, open bool) {
		t.Helper()
		if err := notices.RunChecks(context.Background(), svc.db); err != nil {
			t.Fatalf("the inbox's checks: %v", err)
		}
		var resolved *string
		if err := svc.db.QueryRow(`SELECT agency_id, detail, resolved_at FROM notices WHERE kind='schedule_gone' AND subject='job:j-app:yearly'`).
			Scan(&agency, &detail, &resolved); err != nil {
			return "", "", false
		}
		return agency, detail, resolved == nil
	}
	agency, detail, open := gone()
	if !open || agency != "global" {
		t.Fatalf("the notice for the job whose schedule has gone: open=%v agency=%q", open, agency)
	}
	for _, want := range []string{"The job app-job", "yearly", "removed from its Git repository", first} {
		if !strings.Contains(detail, want) {
			t.Errorf("the notice does not say %q: %s", want, detail)
		}
	}
	// The job is given a timing of its own: the entry names no schedule now.
	if _, err := svc.db.Exec(`UPDATE definition_schedules SET source_ref = NULL, schedule_uid = NULL WHERE owner_uid='j-app'`); err != nil {
		t.Fatal(err)
	}
	if _, _, open := gone(); open {
		t.Errorf("the notice is still open after the job was given a timing of its own")
	}
}
