package scheduler

import (
	"database/sql"
	"testing"
)

// PRESENT DEFECT 47 (a side agent's note of 2026-10-13, after 2.4.0 Phase R5;
// in the released code), FIXED in 2.5.0 C6 on the owner's answer to question 17.
//
// The installation has ONE concurrency cap (settings.maxConcurrent, five by
// default), and until C6 it counted runs that are queued as well as runs that
// are running. A queued run waits for a runner that may take it, nothing ends
// a run that stays queued, and a job allows overlapping runs unless it says
// otherwise. So a scheduled job of ONE agency whose agent was switched off
// left one more waiting run at every fire, and after as many fires as the cap
// allows every other job's fire was skipped, in every agency.
//
// Now (internal/runcap) a waiting run counts only against fires of its own
// agency; running runs count against everyone. The claim's half of the
// decision (the cap asked at the claim too) is pinned in the runner package,
// which this one cannot import.
//
// This test was TestGR0_OneAgencysWaitingRunsHoldTheCapForEveryAgency, the pin
// of the defect, and is inverted here as the plan said it would be.

// twoAgencies seeds Alpha and Beta, a scope and a job of each, Alpha's agent
// registered and switched off and Beta's online and idle.
func twoAgencies(t *testing.T, pool *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'Alpha', 't'), ('ag-b', 'Beta', 't')`,
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('s-a', 'a-hosts', 'cronomicon', 't'), ('s-b', 'b-hosts', 'cronomicon', 't')`,
		`DELETE FROM scope_agencies WHERE scope_id IN ('s-a', 's-b')`,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-a', 'ag-a'), ('s-b', 'ag-b')`,
		`UPDATE scopes SET owner_agency = 'ag-a' WHERE id = 's-a'`,
		`UPDATE scopes SET owner_agency = 'ag-b' WHERE id = 's-b'`,
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
	for job, agency := range map[string]string{"alphas-nightly": "ag-a", "betas-nightly": "ag-b"} {
		if _, err := pool.Exec(`UPDATE jobs SET owner_agency = ? WHERE name = ?`, agency, job); err != nil {
			t.Fatal(err)
		}
	}
}

func skippedReason(t *testing.T, pool *sql.DB, job string) string {
	t.Helper()
	var reason string
	if err := pool.QueryRow(`SELECT COALESCE(queued_reason, '') FROM runs WHERE job_name = ? AND status = 'skipped'`, job).Scan(&reason); err != nil {
		t.Fatalf("%s's fire left no skipped row: %v", job, err)
	}
	return reason
}

func TestAWaitingRunHoldsUpOnlyItsOwnAgency(t *testing.T) {
	pool := mustPool(t)
	twoAgencies(t, pool)
	s := New(pool, quietLog(), nil)
	limit := maxConcurrentDB(ctxb(), pool)
	fireA := func() {
		s.fire("git", "alphas-nightly", "uid-alphas-nightly", "bash", "a-hosts", "Allow", "", "nightly", "")
	}
	fireB := func() {
		s.fire("git", "betas-nightly", "uid-betas-nightly", "bash", "b-hosts", "Allow", "", "nightly", "")
	}

	// Alpha's job fires as often as the cap allows. Nothing takes its runs.
	for i := 0; i < limit; i++ {
		fireA()
	}
	if n := runsOf(t, pool, "alphas-nightly", "queued"); n != limit {
		t.Fatalf("waiting runs of Alpha's job = %d, want %d (one for each fire)", n, limit)
	}

	// THE FIX: Beta's job, whose agent is online and idle, is not held up.
	fireB()
	if n := runsOf(t, pool, "betas-nightly", "queued"); n != 1 {
		t.Fatalf("Beta's fire queued %d runs, want 1: Alpha's waiting runs still hold Beta's fires", n)
	}

	// Alpha's own next fire is: its waiting runs fill the installation's cap
	// for Alpha, and the reason is the installation's cap, in its old words.
	fireA()
	if n := runsOf(t, pool, "alphas-nightly", "queued"); n != limit {
		t.Fatalf("Alpha's fire past the cap queued a run (%d waiting, want %d)", n, limit)
	}
	if reason := skippedReason(t, pool, "alphas-nightly"); reason != reasonConcurrencyCap {
		t.Fatalf("Alpha's capped fire was skipped with %q, want %q", reason, reasonConcurrencyCap)
	}

	// Running runs count against everyone: with the cap's worth running
	// elsewhere (Global's, here), Alpha is held as well.
	if _, err := pool.Exec(`UPDATE runs SET status = 'killed' WHERE job_name = 'alphas-nightly' AND status = 'queued'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < limit; i++ {
		seedActiveRun(t, pool, "busy-"+string(rune('0'+i)), "another-agencys-job")
	}
	before := runsOf(t, pool, "alphas-nightly", "queued")
	fireA()
	if n := runsOf(t, pool, "alphas-nightly", "queued"); n != before {
		t.Errorf("Alpha's fire was queued while the installation's cap was running elsewhere")
	}
}

// QT-1: an agency's own cap counts its runs, waiting and running, where one is
// produced, and refuses with a reason naming the agency. Another agency is not
// held by it, and an agency with no cap of its own is under the installation's
// alone.
func TestAnAgencysOwnCapHoldsItsFiresOnly(t *testing.T) {
	pool := mustPool(t)
	twoAgencies(t, pool)
	if _, err := pool.Exec(`UPDATE agencies SET max_concurrent = 2 WHERE id = 'ag-a'`); err != nil {
		t.Fatal(err)
	}
	s := New(pool, quietLog(), nil)
	fireA := func() {
		s.fire("git", "alphas-nightly", "uid-alphas-nightly", "bash", "a-hosts", "Allow", "", "nightly", "")
	}
	for i := 0; i < 3; i++ {
		fireA()
	}
	if n := runsOf(t, pool, "alphas-nightly", "queued"); n != 2 {
		t.Fatalf("Alpha's waiting runs under a cap of 2 = %d, want 2", n)
	}
	if reason, want := skippedReason(t, pool, "alphas-nightly"), "Skipped: Alpha's concurrency cap was reached"; reason != want {
		t.Fatalf("Alpha's third fire was skipped with %q, want %q", reason, want)
	}
	if !AtCapacity(ctxb(), pool, "ag-a") || AtCapacity(ctxb(), pool, "ag-b") {
		t.Errorf("AtCapacity: Alpha %v, Beta %v; want Alpha held and Beta not", AtCapacity(ctxb(), pool, "ag-a"), AtCapacity(ctxb(), pool, "ag-b"))
	}
	s.fire("git", "betas-nightly", "uid-betas-nightly", "bash", "b-hosts", "Allow", "", "nightly", "")
	if n := runsOf(t, pool, "betas-nightly", "queued"); n != 1 {
		t.Errorf("Beta's fire was held by Alpha's cap")
	}
}
