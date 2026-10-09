package gitlab

import (
	"context"
	"testing"
)

// Present defect 19, read by Phase R0 and reproduced here before Phase R3's
// sync problems change it. It passes on the code as it is.
//
// Two files in ONE repository that define a job, a workflow, a schedule or a
// script of the same name: the one read last wins, in silence. The sync is a
// clean success, nothing is logged, and which file's definition is running is
// a matter of the order the directory was walked in. Migration 1050's header
// says the sync validator refuses a duplicate; only `cronomicon validate`
// does, and sync never calls it.
func TestGR0_ADuplicateNameInOneRepositoryWinsInSilence(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	grCommitFiles(t, repo, remote, map[string]string{
		"jobs/a-first.yaml":      grJob("twice", "echo from-a-first"),
		"jobs/z-last.yaml":       grJob("twice", "echo from-z-last"),
		"schedules/a-first.yaml": grSchedule("0 3 1 1 *"),
		"schedules/z-last.yaml":  grSchedule("0 4 2 2 *"),
	}, "two jobs and two schedules of one name each")
	res := svc.SyncBlocking(context.Background(), "manual")
	if res.Status == "failed" {
		t.Fatalf("sync: %s", res.ErrorMessage)
	}
	if n := jobCount(t, svc.db, "twice"); n != 1 {
		t.Fatalf("jobs named twice = %d, want the one row both files wrote", n)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM schedules WHERE source='git' AND name='yearly'`); n != 1 {
		t.Fatalf("schedules named yearly = %d, want the one row both files wrote", n)
	}
	switch {
	case res.Status == "success" && len(res.Errors) == 0:
		// Today: a clean success, and nothing says that a file was ignored.
	default:
		t.Errorf("the sync is %q with %d problem(s) (%s): a duplicate name is reported now, and the test is to be inverted (Phase R3)",
			res.Status, len(res.Errors), res.ErrorMessage)
	}
}
