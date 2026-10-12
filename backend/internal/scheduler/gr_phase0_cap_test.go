package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestGR0_WaitingCheckoutRunsHoldTheConcurrencyCap pins present defect 44
// (found by the second review of 2.4.0 Phase R5 on 2026-10-12, reproduced
// here, not changed).
//
// The installation-wide concurrency cap counts runs that are queued as well as
// runs that are running (five by default), and nothing expires a queued run.
// Since present defect 39 was fixed, a project job's run WAITS for an agent
// started with -allow-checkout. Where no such agent serves the job, every
// fire of its schedule leaves one more run that will never start, and a job
// allows overlapping runs unless it says otherwise: after five fires the cap
// is reached, and every other job's fire is skipped ("the global concurrency
// cap was reached"). Before, the run was taken by an agent that refused it
// and failed at once, so nothing accumulated.
//
// The same holds, and held before, for any run that waits for something no
// runner has (a requirement token, a become password on an old Ansible): this
// is the cap's rule, and present defect 39's fix gave it a new way in.
//
// To put right, one of: refuse a project job's run where it is produced when
// no REGISTERED agent that serves it allows checkout, as a key-bound shell run
// is refused when no agent could deliver the key (runref.KeyBindingsNeedAgent),
// and let it wait only for an agent that exists; or leave runs that no
// registered runner can take out of the cap's count. The owner's to decide.
func TestGR0_WaitingCheckoutRunsHoldTheConcurrencyCap(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`UPDATE git_repos SET last_sha = '1111111111111111111111111111111111111111' WHERE id = 'global'`)
	// A project job, allowing overlapping runs as a job does by default, and
	// no runner at all that allows checkout.
	exec(`INSERT INTO jobs (uid, name, source, run_type, script_path, project_root, concurrency_policy, synced_at)
	      VALUES ('u-proj', 'proj', 'git', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'Allow', 't')`)
	limit := maxConcurrentDB(ctx, pool)
	if AtCapacity(ctx, pool) {
		t.Fatal("at capacity before anything was queued")
	}
	for i := 0; i < limit; i++ {
		if _, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
			JobName: "proj", JobSource: "git", JobUID: "u-proj", RunType: "ansible", TriggerKind: "scheduled", TriggeredBy: "scheduler",
		}); err != nil {
			t.Fatalf("fire %d: %v", i+1, err)
		}
	}
	var queued int
	var requires sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(requires_json) FROM runs WHERE status = 'queued' AND job_name = 'proj'`).Scan(&queued, &requires); err != nil {
		t.Fatal(err)
	}
	if queued != limit || !strings.Contains(requires.String, "checkout") {
		t.Fatalf("%d of %d fires are queued, requiring %q: the fixture does not hold the runs waiting for checkout", queued, limit, requires.String)
	}
	if !AtCapacity(ctx, pool) {
		t.Errorf("PRESENT DEFECT CHANGED: %d runs that wait for an agent that allows checkout no longer reach the cap of %d. "+
			"If such runs are refused where they are produced, or left out of the count, invert this test.", queued, limit)
	}
}
