package scheduler

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// Until this test was inverted (present defect 34, found on 2026-10-10 while
// grounding 2.4.0's Phase R4; in the released code since two workflows could
// share a name, 2.2.x; TestGR0_TwoWorkflowsOfOneNameFireAsOne pinned it) a
// workflow's schedule entry reached whatever starts the workflow as a source
// and a NAME, and nothing else: the scheduler read no identity for a workflow
// entry, and the callback it was given (WorkflowFirer) had nowhere to carry
// one. The server's callback then found the workflow with
//
//	SELECT rowid, steps FROM workflows WHERE source = ? AND name = ?
//
// Two agencies may each hold a workflow built in the app of one name (R2-5),
// and since 2.4.0 two repositories may each hold a Git one. Both workflows'
// schedules then started the SAME workflow: one agency's schedule ran the
// other agency's steps, and its own workflow never ran on its schedule.
//
// Now the entry carries the uid of the workflow it belongs to, and each fire
// hands the firer its own. What the server does with it is
// workflow.LoadForFire, which has its own tests.
func TestTwoWorkflowsOfOneNameEachFireAsThemselves(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedTwoNightlies(t, pool)

	s := New(pool, quietLog(), nil)
	var mu sync.Mutex
	started := map[string]WorkflowFire{} // schedule entry → what the firer was handed
	s.SetWorkflowFirer(func(_ context.Context, f WorkflowFire) {
		mu.Lock()
		started[f.ScheduleName] = f
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
	for sched, want := range map[string]string{"fin-at-two": "w-fin", "tax-at-three": "w-tax"} {
		f := started[sched]
		if f.UID != want {
			t.Errorf("the schedule %s handed the firer the workflow %q, want its own, %s", sched, f.UID, want)
		}
		if f.Name != "nightly" || f.Source != "cronomicon" {
			t.Errorf("the schedule %s: the fire is called %s/%s, want cronomicon/nightly", sched, f.Source, f.Name)
		}
	}
}

// seedTwoNightlies is two agencies' workflows of one name, each with its own
// steps and its own schedule entry.
func seedTwoNightlies(t *testing.T, pool *sql.DB) {
	t.Helper()
	seedFinTax(t, pool)
	for _, q := range []string{
		`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency) VALUES
		      ('w-fin', 'nightly', 'cronomicon', '[{"type":"job","name":"fin-job"}]', 1, 't', 'ag-fin'),
		      ('w-tax', 'nightly', 'cronomicon', '[{"type":"job","name":"tax-job"}]', 1, 't', 'ag-tax')`,
		`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES
		      ('cronomicon', 'workflow', 'nightly', 'fin-at-two',   '0 2 * * *', 0, 'w-fin'),
		      ('cronomicon', 'workflow', 'nightly', 'tax-at-three', '0 3 * * *', 0, 'w-tax')`,
	} {
		if _, err := pool.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
}

// A fire that does not happen (here: the workflow is paused) leaves a row, and the
// row is stamped with the workflow whose fire it was. It was stamped with
// whichever workflow the name found.
func TestASuppressedWorkflowFireIsStampedWithItsOwnWorkflow(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedTwoNightlies(t, pool)
	if _, err := pool.ExecContext(ctx, `INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES ('cronomicon', 'workflow', 'nightly', 't', 't')`); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s := New(pool, quietLog(), nil)
	s.SetWorkflowFirer(func(context.Context, WorkflowFire) { t.Error("a paused workflow was fired") })
	s.fireWorkflow("w-tax", "cronomicon", "nightly", "tax-at-three", "")

	var uid sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT workflow_uid FROM workflow_runs WHERE status = 'skipped' AND schedule_name = 'tax-at-three'`).Scan(&uid); err != nil {
		t.Fatalf("the skipped row: %v", err)
	}
	if uid.String != "w-tax" {
		t.Fatalf("the skipped fire of w-tax's schedule is stamped %q, want w-tax", uid.String)
	}
}

// A caller that has only the name (the missed-fire detector) stamps the uid
// when one workflow holds the name, and nothing when two do: a row that names
// nobody, not the wrong one.
func TestASuppressedWorkflowFireWithNoIdentityDoesNotGuess(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedTwoNightlies(t, pool)
	if _, err := pool.ExecContext(ctx, `INSERT INTO workflows (uid, name, source, steps, enabled, created_at)
		VALUES ('w-solo', 'weekly', 'cronomicon', '[]', 1, 't')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := New(pool, quietLog(), nil)
	for _, name := range []string{"nightly", "weekly"} {
		if err := s.recordWorkflowSuppression(ctx, "", "cronomicon", name, "s", skipRecord{
			Reason: reasonMissedFire, Mode: dedupeEpisodeReason, At: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	for name, want := range map[string]string{"nightly": "", "weekly": "w-solo"} {
		var uid sql.NullString
		if err := pool.QueryRowContext(ctx,
			`SELECT workflow_uid FROM workflow_runs WHERE status = 'skipped' AND workflow_name = ?`, name).Scan(&uid); err != nil {
			t.Fatalf("the skipped row of %s: %v", name, err)
		}
		if uid.String != want {
			t.Errorf("the skipped fire of %s is stamped %q, want %q", name, uid.String, want)
		}
	}
}
