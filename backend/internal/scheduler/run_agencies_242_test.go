package scheduler

import "testing"

// Present defect 66, its second half (found by the review of 2.4.x): a run
// whose agencies could not be written was left queued, claimable by no runner
// and counted toward the cap until retention. It is closed, failed, with the
// reason.
func TestRV_ARunWhoseAgenciesCannotBeWrittenIsClosed(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "nightly", 1)
	_, err := EnqueueRunWithID(ctxb(), pool, EnqueueParams{
		JobName: "nightly", JobSource: "git", JobUID: "uid-nightly", RunType: "bash",
		TriggerKind: "manual", TriggeredBy: "t", AgenciesJSON: `["Global"`,
	})
	if err == nil {
		t.Fatalf("an agency snapshot that cannot be read was enqueued")
	}
	var status, reason string
	if qerr := pool.QueryRow(`SELECT status, COALESCE(queued_reason, '') FROM runs WHERE job_name = 'nightly'`).Scan(&status, &reason); qerr != nil {
		t.Fatalf("the run row: %v (the test needs InsertRun to write it and SyncRunAgencies to fail)", qerr)
	}
	if status != "failure" || reason != ReasonRunAgenciesUnwritten {
		t.Errorf("the run whose agencies were not written is %q, %q; want failure, %q", status, reason, ReasonRunAgenciesUnwritten)
	}
}
