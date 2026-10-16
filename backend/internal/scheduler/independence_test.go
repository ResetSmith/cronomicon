package scheduler

import (
	"testing"
	"time"
)

// A reaction watches its own agency's definitions (§0a, IN-8). One that is
// pinned to another agency's definition (written before §0a, or left so by a
// scope given to another agency) does not fire when that definition's run
// finishes, and its delivery says why (§0a, open point 1). Its own agency's
// upstream still fires it.
//
// Until §0a a reaction pinned by identity fired on whichever agency's
// definition it named.
func TestI1_AReactionToAnotherAgencysDefinitionDoesNotFire(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-b', 'b-hosts', 'cronomicon', 't'), ('sc-c', 'c-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id IN ('sc-b', 'sc-c')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-b', 'ag-b'), ('sc-c', 'ag-c')`)
	job := func(uid, scope string) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, scope)
		      VALUES (?, ?, 'cronomicon', 'bash', 'Allow', 1, 't', ?)`, uid, uid, scope)
	}
	job("after", "b-hosts")  // B's
	job("theirs", "c-hosts") // C's
	job("ours", "b-hosts")   // B's
	for _, up := range []string{"theirs", "ours"} {
		exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_uid, on_outcome,
		                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, owner_uid)
		      VALUES ('cronomicon', 'job', 'after', ?, 'cronomicon', 'job', ?, ?, 'success', 0, 0, 0, 1, 'after')`, "on-"+up, up, up)
	}
	primeCursor(t, s)
	finish := func(uid string) {
		t.Helper()
		ts := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
		exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, completed_at, created_at)
		      VALUES (?, ?, 'cronomicon', ?, 'bash', 'success', 't', 'scheduled', ?, ?)`, "r-"+uid, uid, uid, ts, ts)
	}
	finish("theirs")
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "after"); n != 0 {
		t.Errorf("another agency's job ran: the reaction fired %d time(s), want 0", n)
	}
	var said bool
	for _, d := range deliveries(t, pool, "after") {
		if d.Result == "error" && d.Detail == ReasonReactionOtherAgency {
			said = true
		}
	}
	if !said {
		t.Errorf("the refused delivery does not say why: %+v", deliveries(t, pool, "after"))
	}

	finish("ours")
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "after"); n != 1 {
		t.Errorf("its own agency's job ran: the reaction fired %d time(s), want 1", n)
	}
}
