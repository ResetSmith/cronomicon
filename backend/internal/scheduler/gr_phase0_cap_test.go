package scheduler

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// Present defect 44 (this file held its pin,
// TestGR0_WaitingCheckoutRunsHoldTheConcurrencyCap, inverted on the owner's
// decision of 2026-10-13), on the three producers the scheduler owns.
//
// The installation-wide concurrency cap counts runs that are queued as well as
// runs that are running, and nothing expires a queued run. A project job's
// run waits for an agent started with -allow-checkout (present defect 39).
// Where no such agent serves the job, every fire left one more run that would
// never start, and after as many fires as the cap allows every other job's
// fire was skipped. Such a run is refused where it is produced; it waits only
// for an agent that is registered.

// projectJob makes job a project (uid "uid-<job>", as seedJobRow gives it).
func projectJob(t *testing.T, pool *sql.DB, job string) {
	t.Helper()
	seedJobRow(t, pool, job, 1)
	if _, err := pool.Exec(`UPDATE git_repos SET last_sha = '1111111111111111111111111111111111111111' WHERE id = 'global'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`UPDATE jobs SET run_type = 'ansible', script_path = 'scripts/proj/site.yml', project_root = 'scripts/proj' WHERE name = ?`, job); err != nil {
		t.Fatal(err)
	}
}

// checkoutAgent registers an agent with the given capabilities, offline. It is
// opsAgency's, the agency seedJobRow's jobs are: a run with no scope is its
// job's agency's (2.5.3, CA-5; Global's before, and so was this agent), and an
// agent serves its owner's work only.
func checkoutAgent(t *testing.T, pool *sql.DB, id string, caps string) {
	t.Helper()
	seedOpsAgency(t, pool)
	if _, err := pool.Exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
	      VALUES (?, ?, 'agent', 'offline', 'Linux', ?, 0, 5, '1.0', 't', 't', ?)`, id, id, caps, opsAgency); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	if _, err := pool.Exec(`INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, id, opsAgency); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledFiresOfAProjectJobWithNoCheckoutAgentDoNotFillTheCap(t *testing.T) {
	pool := mustPool(t)
	projectJob(t, pool, "proj")
	seedJobRow(t, pool, "plain", 1)
	s := New(pool, quietLog(), nil)
	fire := func(job, runType string) { s.fire("git", job, "uid-"+job, runType, "", "Allow", "", "nightly", "") }
	limit := maxConcurrentDB(ctxb(), pool)

	// More fires than the cap allows, with nothing registered that allows
	// checkout: none is queued, one row says why, and the cap is untouched.
	for i := 0; i < limit+2; i++ {
		fire("proj", "ansible")
	}
	if n := runsOf(t, pool, "proj", "queued"); n != 0 {
		t.Fatalf("%d runs of a project job that no agent can take are queued, want none", n)
	}
	var reason string
	if err := pool.QueryRow(`SELECT COALESCE(queued_reason,'') FROM runs WHERE job_name='proj' AND status='skipped'`).Scan(&reason); err != nil {
		t.Fatalf("no skipped run recorded: the fires vanished silently: %v", err)
	}
	if reason != runref.ReasonCheckoutNeedsAgent {
		t.Errorf("queued_reason = %q, want %q", reason, runref.ReasonCheckoutNeedsAgent)
	}
	if n := runsOf(t, pool, "proj", "skipped"); n != 1 {
		t.Errorf("skipped rows after %d refused fires of one day = %d, want 1 (a standing refusal)", limit+2, n)
	}
	if AtCapacity(ctxb(), pool, "global") {
		t.Fatalf("the installation is at capacity after fires that were all refused")
	}
	// Another job's fire is not held up.
	fire("plain", "bash")
	if n := runsOf(t, pool, "plain", "queued"); n != 1 {
		t.Errorf("queued runs of another job = %d, want 1: the refused project held it up", n)
	}

	// An agent that does not allow checkout changes nothing.
	checkoutAgent(t, pool, "plain-agent", `["ansible","bash"]`)
	fire("proj", "ansible")
	if n := runsOf(t, pool, "proj", "queued"); n != 0 {
		t.Errorf("a project job's run was queued for an agent that does not allow checkout")
	}

	// One that allows it is registered, and offline: the run waits for it, as
	// any run waits for its runner, and asks for the token.
	checkoutAgent(t, pool, "checkout-agent", `["ansible","checkout"]`)
	fire("proj", "ansible")
	if n := runsOf(t, pool, "proj", "queued"); n != 1 {
		t.Fatalf("queued runs of the project job with an agent to wait for = %d, want 1", n)
	}
	var requires sql.NullString
	if err := pool.QueryRow(`SELECT requires_json FROM runs WHERE job_name='proj' AND status='queued'`).Scan(&requires); err != nil || !strings.Contains(requires.String, "checkout") {
		t.Errorf("the waiting run requires %q (%v), want the checkout token", requires.String, err)
	}
}

func TestReactionToAProjectJobWithNoCheckoutAgentRecordsADeliveryErrorNotARun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	projectJob(t, pool, "downstream")
	seedReaction(t, pool, "downstream", "on-upstream", "upstream", "success")
	primeCursor(t, s)

	seedFinishedRun(t, pool, "r1", "upstream", "success", time.Minute)
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "downstream"); n != 0 {
		t.Errorf("pending runs of a project job that no agent can take = %d, want none", n)
	}
	var refused int
	for _, d := range deliveries(t, pool, "downstream") {
		if d.Result == "error" && strings.Contains(d.Detail, "-allow-checkout") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("deliveries = %+v, want one error saying no agent allows checkout", deliveries(t, pool, "downstream"))
	}

	// With an agent to wait for, the reaction parks its run.
	checkoutAgent(t, pool, "checkout-agent", `["ansible","checkout"]`)
	seedFinishedRun(t, pool, "r2", "upstream", "success", time.Second)
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "downstream"); n != 1 {
		t.Errorf("pending runs with an agent to wait for = %d, want 1", n)
	}
}

func TestParkedRunOfAProjectJobWithNoCheckoutAgentIsMissedNotEnqueued(t *testing.T) {
	pool := mustPool(t)
	projectJob(t, pool, "proj")
	s := New(pool, quietLog(), nil)
	park := func() string {
		t.Helper()
		id, err := InsertPendingRun(ctxb(), pool, "job", "proj", "git", "",
			time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "op@example.com",
			&EnqueueParams{JobName: "proj", JobSource: "git", JobUID: "uid-proj", RunType: "ansible",
				TriggerKind: "manual", TriggeredBy: "op@example.com"})
		if err != nil {
			t.Fatalf("park: %v", err)
		}
		return id
	}

	id := park()
	s.PromotePending(ctxb())
	if n := runsOf(t, pool, "proj", "queued"); n != 0 {
		t.Errorf("a parked run of a project job that no agent can take was promoted (queued = %d)", n)
	}
	var status, reason string
	if err := pool.QueryRow(`SELECT status, COALESCE(miss_reason,'') FROM pending_runs WHERE id = ?`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "missed" || reason != runref.ReasonCheckoutNeedsAgent {
		t.Errorf("the parked run is %q with reason %q; want missed, %q", status, reason, runref.ReasonCheckoutNeedsAgent)
	}

	checkoutAgent(t, pool, "checkout-agent", `["ansible","checkout"]`)
	park()
	s.PromotePending(ctxb())
	if n := runsOf(t, pool, "proj", "queued"); n != 1 {
		t.Errorf("a parked run with an agent to wait for was not promoted (queued = %d)", n)
	}
}
