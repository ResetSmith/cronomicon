package scheduler

import (
	"testing"
)

// PRESENT DEFECT 47, reproduced and not changed (a side agent's note of
// 2026-10-13, after 2.4.0 Phase R5; in the released code).
//
// The installation has ONE concurrency cap (settings.maxConcurrent, five by
// default), and it counts runs that are queued as well as runs that are
// running (activeRunCount, AtCapacity). A queued run waits for a runner that
// may take it, and nothing ends a run that stays queued. A job allows
// overlapping runs unless it says otherwise (concurrency_policy 'Allow').
//
// So a scheduled job of ONE agency whose agent is switched off leaves one more
// waiting run at every fire, and after as many fires as the cap allows, every
// other job's scheduled fire is skipped, in every agency, until somebody ends
// the waiting runs or the agent comes back.
//
// Present defect 44 was this for a project job with no agent that allows
// checkout, and its fix (8b7f857) refuses a run that no REGISTERED agent could
// take. An agent that is registered and offline is an ordinary wait, there and
// here. This is every kind of job.
//
// Not changed: whether a waiting run counts, whether the cap is per agency
// (2.5.0's Phase C6 plans a cap per agency), and whether a run that waits too
// long is ended, are the owner's (question 17). When it is decided, this test
// is inverted.
func TestGR0_OneAgencysWaitingRunsHoldTheCapForEveryAgency(t *testing.T) {
	pool := mustPool(t)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'Alpha', 't'), ('ag-b', 'Beta', 't')`,
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('s-a', 'a-hosts', 'cronomicon', 't'), ('s-b', 'b-hosts', 'cronomicon', 't')`,
		`DELETE FROM scope_agencies WHERE scope_id IN ('s-a', 's-b')`,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-a', 'ag-a'), ('s-b', 'ag-b')`,
		// Alpha's agent is registered and switched off; Beta's is online and idle.
		`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at) VALUES
		   ('agent-a', 'agent-a', 'agent', 'offline', 'Linux', '["bash"]', 0, 5, '1.0', 't', 't'),
		   ('agent-b', 'agent-b', 'agent', 'online',  'Linux', '["bash"]', 0, 5, '1.0', 't', 't')`,
		`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('agent-a', 'ag-a'), ('agent-b', 'ag-b')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seedJobRow(t, pool, "alphas-nightly", 1)
	seedJobRow(t, pool, "betas-nightly", 1)
	s := New(pool, quietLog(), nil)
	limit := maxConcurrentDB(ctxb(), pool)

	// Alpha's job fires as often as the cap allows. Nothing takes its runs.
	for i := 0; i < limit; i++ {
		s.fire("git", "alphas-nightly", "uid-alphas-nightly", "bash", "a-hosts", "Allow", "", "nightly", "")
	}
	if n := runsOf(t, pool, "alphas-nightly", "queued"); n != limit {
		t.Fatalf("waiting runs of Alpha's job = %d, want %d (one for each fire)", n, limit)
	}

	// THE DEFECT: Beta's job, whose agent is online and idle, does not run.
	s.fire("git", "betas-nightly", "uid-betas-nightly", "bash", "b-hosts", "Allow", "", "nightly", "")
	if n := runsOf(t, pool, "betas-nightly", "queued"); n != 0 {
		t.Fatalf("Beta's fire was queued (%d): the defect as pinned is that it is skipped. "+
			"If this changed on purpose, invert this test", n)
	}
	var reason string
	if err := pool.QueryRow(`SELECT COALESCE(queued_reason, '') FROM runs WHERE job_name = 'betas-nightly' AND status = 'skipped'`).Scan(&reason); err != nil {
		t.Fatalf("Beta's fire left no skipped row: %v", err)
	}
	if reason != reasonConcurrencyCap {
		t.Fatalf("Beta's fire was skipped with %q, want %q", reason, reasonConcurrencyCap)
	}
}
