package scheduler

import (
	"context"
	"sync"
	"testing"
)

// PINNED, present defect 34 (found on 2026-10-10 while grounding 2.4.0's Phase
// R4; in the released code since two workflows could share a name, 2.2.x).
//
// A workflow's schedule entry reaches whatever starts the workflow as a source
// and a NAME, and nothing else: the scheduler reads no identity for a workflow
// entry, and the callback it is given (WorkflowFirer) has nowhere to carry
// one. The server's callback then finds the workflow with
//
//	SELECT rowid, steps FROM workflows WHERE source = ? AND name = ?
//
// Two agencies may each hold a workflow built in the app of one name (R2-5),
// and since 2.4.0 two repositories may each hold a Git one. Both workflows'
// schedules then start the SAME workflow: one agency's schedule runs the other
// agency's steps, and its own workflow never runs on its schedule. The
// deferred-run callback (PendingWorkflowFirer) is the same.
//
// It is the workflow form of present defect 4 (a run of a job handed another
// agency's same-named job's body) and belongs with it in Phase R5: the entry
// and the callback carry the workflow's uid, and the callback reads the row by
// it. When that is done this test is inverted: each fire names its own
// workflow.
func TestGR0_TwoWorkflowsOfOneNameFireAsOne(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Two agencies' workflows of one name, each with its own steps and its own
	// schedule entry.
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at) VALUES
	      ('w-fin', 'nightly', 'cronomicon', '[{"type":"job","name":"fin-job"}]', 1, 't'),
	      ('w-tax', 'nightly', 'cronomicon', '[{"type":"job","name":"tax-job"}]', 1, 't')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES
	      ('cronomicon', 'workflow', 'nightly', 'fin-at-two',   '0 2 * * *', 0, 'w-fin'),
	      ('cronomicon', 'workflow', 'nightly', 'tax-at-three', '0 3 * * *', 0, 'w-tax')`)

	s := New(pool, quietLog(), nil)
	var mu sync.Mutex
	started := map[string]string{} // schedule entry → the workflow the server's lookup finds for it
	s.SetWorkflowFirer(func(ctx context.Context, source, workflowName, scheduleName, envJSON string) {
		// What cmd/cronomicon's callback does with what it is given.
		var uid string
		if err := pool.QueryRowContext(ctx, `SELECT uid FROM workflows WHERE source = ? AND name = ?`, source, workflowName).Scan(&uid); err != nil {
			t.Errorf("the lookup for %s: %v", scheduleName, err)
			return
		}
		mu.Lock()
		started[scheduleName] = uid
		mu.Unlock()
	})
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entries := s.cr.Entries()
	if len(entries) != 2 {
		t.Fatalf("registered %d entries, want the two workflows' one each", len(entries))
	}
	for _, e := range entries {
		e.Job.Run()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 {
		t.Fatalf("the callback was reached for %d of the 2 schedules: %v", len(started), started)
	}
	if started["fin-at-two"] != started["tax-at-three"] {
		t.Fatalf("PIN BROKEN: the two schedules started different workflows (%v). Present defect 34 is fixed: "+
			"invert this test (want fin-at-two to start w-fin and tax-at-three to start w-tax)", started)
	}
}
