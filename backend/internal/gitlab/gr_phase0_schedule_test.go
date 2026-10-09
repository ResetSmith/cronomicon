package gitlab

import (
	"testing"
)

// Present defect 13, reproduced before the schedules half of Phase R1 changes
// anything. It passes on the code as it is. No production code changes.
//
// A definition bound to a reusable schedule holds a COPY of it in
// definition_schedules. Sync writes that copy afresh for a Git job or workflow
// on every sync. Nothing writes it for a job built in the app: the in-app edit
// of an in-app schedule propagates to its users, but a Git edit of a Git
// schedule reaches only the Git definitions. So an in-app job bound to Git's
// `nightly` goes on firing at the old time after `nightly` is changed in Git,
// and goes on firing at all after `nightly` is removed from Git, on a
// reference that now names nothing.
func TestGR0_AGitSchedulesEditDoesNotReachInAppDefinitions(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	grCommitFiles(t, repo, remote, map[string]string{
		"schedules/yearly.yaml": grSchedule("0 3 1 1 *"),
		"jobs/git-job.yaml":     "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: git-job\nspec:\n  run_type: bash\n  command: echo hi\n  scheduleRefs:\n    - yearly\n",
	}, "a schedule and a Git job bound to it")
	grSync(t, svc, "first sync")
	sched := grString(t, svc.db, `SELECT uid FROM schedules WHERE source='git' AND name='yearly'`)
	cronOf := func(source, owner string) string {
		t.Helper()
		if grCount(t, svc.db, `SELECT COUNT(*) FROM definition_schedules WHERE owner_source=? AND owner_name=?`, source, owner) == 0 {
			return "(no entry)"
		}
		return grString(t, svc.db, `SELECT cron FROM definition_schedules WHERE owner_source=? AND owner_name=?`, source, owner)
	}
	first := cronOf("git", "git-job")

	// An in-app job bound to the same Git schedule, as the composer writes it:
	// a copy of the schedule's timing, its name, and its uid.
	exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES('j-app', 'app-job', 'cronomicon', 'bash', 'echo hi', 't')`)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	      VALUES('cronomicon', 'job', 'app-job', 'yearly', ?, 0, 'yearly', 'j-app', ?)`, first, sched)

	// The schedule is edited in Git.
	gitCommitFile(t, repo, remote, "schedules/yearly.yaml", grSchedule("0 4 2 2 *"), "edit the schedule")
	grSync(t, svc, "sync after the edit")
	second := cronOf("git", "git-job")
	if second == first {
		t.Fatalf("the Git job's entry did not follow the edit (%q)", second)
	}
	switch got := cronOf("cronomicon", "app-job"); got {
	case first:
		// Today: the in-app job still fires at the old time.
	case second:
		t.Errorf("the in-app job follows the Git schedule's edit: this is fixed, and the test is to be inverted")
	default:
		t.Errorf("the in-app job's entry is %q: neither the old timing nor the new", got)
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
	switch got := cronOf("cronomicon", "app-job"); got {
	case first:
		// Today: the in-app job still fires, bound to a schedule that is gone.
		if ref := grString(t, svc.db, `SELECT source_ref FROM definition_schedules WHERE owner_name='app-job'`); ref != "yearly" {
			t.Errorf("the orphaned entry's source_ref = %q", ref)
		}
	case "(no entry)":
		t.Errorf("the in-app job was detached when its schedule left Git: this has been decided and built, and the test is to be re-read")
	default:
		t.Errorf("the in-app job's entry is %q after its schedule was pruned", got)
	}
}
