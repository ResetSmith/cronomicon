package scheduler

import (
	"testing"
	"time"
)

// Present defect 60 (found by the review of 2.4.x): the reactor compared a
// reaction's agency with its upstream's only when both had an identity, so a
// reaction with no owner identity (or matched to a run that recorded none)
// fired across agencies. Now each side is told by its name when one
// definition holds it, and a reaction that cannot be told is refused. A
// reaction by name within one agency still fires.
func TestRV_AReactionWithNoIdentityIsJudgedByItsNames(t *testing.T) {
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
	job := func(uid, scope, owner string) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, scope, owner_agency)
		      VALUES (?, ?, 'cronomicon', 'bash', 'Allow', 1, 't', ?, ?)`, uid, uid, scope, owner)
	}
	job("after", "b-hosts", "ag-b")
	job("theirs", "c-hosts", "ag-c")
	job("ours", "b-hosts", "ag-b")
	// Reactions written before identities: no owner uid, no upstream uid.
	for _, up := range []string{"theirs", "ours"} {
		exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
		                             delay_seconds, min_interval_seconds, include_workflow_children, enabled)
		      VALUES ('cronomicon', 'job', 'after', ?, 'cronomicon', 'job', ?, 'success', 0, 0, 0, 1)`, "on-"+up, up)
	}
	primeCursor(t, s)
	finish := func(uid string) {
		t.Helper()
		ts := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
		exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, completed_at, created_at)
		      VALUES (?, ?, 'cronomicon', 'bash', 'success', 't', 'scheduled', ?, ?)`, "r-"+uid, uid, ts, ts)
	}
	finish("theirs")
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "after"); n != 0 {
		t.Errorf("a reaction with no identity fired on another agency's job %d time(s), want 0 (present defect 60)", n)
	}
	finish("ours")
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "after"); n != 1 {
		t.Errorf("a reaction with no identity on its own agency's job fired %d time(s), want 1", n)
	}
}
