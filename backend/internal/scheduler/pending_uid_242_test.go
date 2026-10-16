package scheduler

import (
	"testing"
	"time"
)

// Present defect 56 (found by the review of 2.4.x): a deferred manual run of a
// job was parked with the identity its NAME gave (none, when two agencies hold
// it), though the caller had the job's own. Promoted, it was judged by name:
// a pause on the other agency's twin held it.
func TestRV_ADeferredRunIsParkedAsItsOwnJobs(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't'), ('ag-b', 'B', 't')`)
	for _, j := range [][2]string{{"j-a", "ag-a"}, {"j-b", "ag-b"}} {
		exec(`INSERT INTO jobs (uid, name, source, run_type, command, concurrency_policy, enabled, synced_at, owner_agency)
		      VALUES (?, 'nightly', 'cronomicon', 'bash', 'echo hi', 'Allow', 1, 't', ?)`, j[0], j[1])
	}
	// B's twin is paused.
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid)
	      VALUES ('cronomicon', 'job', 'nightly', 'b-admin', 't', 'j-b')`)

	id, err := InsertPendingRun(ctxb(), pool, "job", "nightly", "cronomicon", "",
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "a-admin",
		&EnqueueParams{JobName: "nightly", JobSource: "cronomicon", JobUID: "j-a", RunType: "bash",
			TriggerKind: "manual", TriggeredBy: "a-admin"})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	var owner string
	if err := pool.QueryRow(`SELECT COALESCE(owner_uid, '') FROM pending_runs WHERE id = ?`, id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "j-a" {
		t.Errorf("A's deferred run is parked as %q's, want A's job j-a", owner)
	}
	s.PromotePending(ctxb())
	var queued int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-a'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		var status, reason string
		_ = pool.QueryRow(`SELECT status, COALESCE(hold_reason, COALESCE(miss_reason, '')) FROM pending_runs WHERE id = ?`, id).Scan(&status, &reason)
		t.Errorf("A's deferred run was not promoted (%d runs; the parked row is %q, %q): B's pause held it", queued, status, reason)
	}
}
