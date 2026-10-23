package scheduler

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// The scheduled fire reads the bindings of the job's SCRIPT by the uid on the
// job (jobs.script_uid, migration 1290). A script that binds a department's
// secret refuses the unbound fire; another script of the same name does not.
func TestScheduledUnboundFireFollowsTheJobsOwnScript(t *testing.T) {
	pool, exec := unboundFireDB(t)
	ctx := context.Background()
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib','global','lib.sh','bash','x','h','t')`)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib-other','repo-b','lib.sh','bash','x','h','t')`)
	exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,owner_uid,ref_kind,ref_name,created_at)
	      VALUES('script','','lib.sh','uid-lib','secret','DEPT_PASSWORD','t')`)
	last := func() (status, reason string) {
		t.Helper()
		if err := pool.QueryRowContext(ctx,
			`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE job_name='unscoped-job' ORDER BY rowid DESC LIMIT 1`).
			Scan(&status, &reason); err != nil {
			t.Fatalf("no run row recorded: %v", err)
		}
		return status, reason
	}

	exec(`UPDATE jobs SET script_ref='lib.sh', script_uid='uid-lib' WHERE name='unscoped-job'`)
	fireUnscoped(t, pool)
	if status, reason := last(); status != "skipped" || reason != runref.QueuedReasonUnboundReferences {
		t.Fatalf("a fire whose script binds an owned secret: status %q, reason %q; want skipped, %q", status, reason, runref.QueuedReasonUnboundReferences)
	}

	exec(`DELETE FROM runs`)
	exec(`UPDATE jobs SET script_uid='uid-lib-other' WHERE name='unscoped-job'`)
	fireUnscoped(t, pool)
	if status, reason := last(); status != "queued" {
		t.Errorf("a fire was not enqueued (status %q, reason %q) for a binding of ANOTHER script that shares its script's name", status, reason)
	}
}
