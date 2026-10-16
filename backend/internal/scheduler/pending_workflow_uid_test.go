package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// The parked half of present defect 34. A parked workflow run was promoted on
// its source and its name, and fired whichever workflow the server's lookup
// found: with two agencies' workflows of one name, possibly the other's.

// parkWorkflow parks a due workflow run, with or without the workflow's uid.
func parkWorkflow(t *testing.T, pool *sql.DB, id, name, ownerUID string) {
	t.Helper()
	var uid any
	if ownerUID != "" {
		uid = ownerUID
	}
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO pending_runs (id, kind, name, source, run_at, scheduled_by, created_at, status, owner_uid)
		VALUES (?, 'workflow', ?, 'cronomicon', ?, 'someone', ?, 'pending', ?)`, id, name, past, past, uid); err != nil {
		t.Fatalf("park %s: %v", id, err)
	}
}

func parkedState(t *testing.T, pool *sql.DB, id string) (status, reason string) {
	t.Helper()
	var st, r sql.NullString
	err := pool.QueryRowContext(context.Background(),
		`SELECT status, miss_reason FROM pending_runs WHERE id = ?`, id).Scan(&st, &r)
	if err == sql.ErrNoRows {
		return "fired", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return st.String, r.String
}

// A parked run that records its workflow fires that workflow, whatever else
// is called the same.
func TestAParkedWorkflowRunFiresTheWorkflowItWasParkedFor(t *testing.T) {
	pool := mustPool(t)
	seedTwoNightlies(t, pool)
	parkWorkflow(t, pool, "p-tax", "nightly", "w-tax")
	parkWorkflow(t, pool, "p-fin", "nightly", "w-fin")

	s := New(pool, quietLog(), nil)
	var got []string
	s.SetPendingWorkflowFirer(func(_ context.Context, f PendingWorkflowFire) { got = append(got, f.UID) })
	s.PromotePending(context.Background())

	if len(got) != 2 || !(got[0] != got[1] && (got[0] == "w-tax" || got[0] == "w-fin") && (got[1] == "w-tax" || got[1] == "w-fin")) {
		t.Fatalf("the two parked runs fired %v, want w-tax and w-fin, one each", got)
	}
}

// A parked run that records no workflow, of a name two workflows hold, is not
// fired: nothing says which was meant. It is marked missed, with the reason.
// One of a name that a single workflow holds fires that workflow.
func TestAParkedWorkflowRunWithNoIdentityDoesNotGuess(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedTwoNightlies(t, pool)
	if _, err := pool.ExecContext(ctx, `INSERT INTO workflows (uid, name, source, steps, enabled, created_at)
		VALUES ('w-solo', 'weekly', 'cronomicon', '[]', 1, 't')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	parkWorkflow(t, pool, "p-two", "nightly", "")
	parkWorkflow(t, pool, "p-one", "weekly", "")
	parkWorkflow(t, pool, "p-gone", "nightly", "w-deleted")

	s := New(pool, quietLog(), nil)
	var got []string
	s.SetPendingWorkflowFirer(func(_ context.Context, f PendingWorkflowFire) { got = append(got, f.Name+"="+f.UID) })
	s.PromotePending(ctx)

	if len(got) != 1 || got[0] != "weekly=w-solo" {
		t.Fatalf("fired %v, want only the run of the one workflow called weekly, as w-solo", got)
	}
	if st, reason := parkedState(t, pool, "p-two"); st != "missed" || reason != ReasonPendingWorkflowAmbiguous {
		t.Errorf("the run of a name two workflows hold is %s (%q), want missed with the reason that says so", st, reason)
	}
	// A recorded workflow that has gone is not replaced by another of its name.
	if st, reason := parkedState(t, pool, "p-gone"); st != "missed" || reason != "workflow no longer exists" {
		t.Errorf("the run of a workflow that no longer exists is %s (%q), want missed", st, reason)
	}
}

// A reaction that fires a workflow parks the run for ITS workflow: the
// reaction row holds the workflow's uid. It was left to the name, which
// records nobody when two workflows hold it.
func TestAReactionParksItsOwnWorkflow(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedTwoNightlies(t, pool)
	s := New(pool, quietLog(), nil)

	id, err := s.fireReaction(ctx, srcEvent{kind: "job", source: "cronomicon", name: "upstream", runID: "r1"},
		reactionRow{ownerSource: "cronomicon", ownerKind: "workflow", ownerName: "nightly", ownerUID: "w-tax", name: "after-upstream"},
		time.Now().UTC())
	if err != nil {
		t.Fatalf("fire the reaction: %v", err)
	}
	var uid sql.NullString
	if err := pool.QueryRowContext(ctx, `SELECT owner_uid FROM pending_runs WHERE id = ?`, id).Scan(&uid); err != nil {
		t.Fatalf("the parked row: %v", err)
	}
	if uid.String != "w-tax" {
		t.Fatalf("the reaction of w-tax parked a run for %q, want w-tax", uid.String)
	}

	// And a reaction whose own workflow is in the recycle bin is an error of
	// the delivery, although another workflow of the name is live.
	if _, err := pool.ExecContext(ctx, `UPDATE workflows SET deleted_at = 't' WHERE uid = 'w-tax'`); err != nil {
		t.Fatalf("bin w-tax: %v", err)
	}
	if _, err := s.fireReaction(ctx, srcEvent{kind: "job", source: "cronomicon", name: "upstream", runID: "r2"},
		reactionRow{ownerSource: "cronomicon", ownerKind: "workflow", ownerName: "nightly", ownerUID: "w-tax", name: "after-upstream"},
		time.Now().UTC()); err == nil {
		t.Fatalf("a reaction whose workflow is binned parked a run, because another workflow has its name")
	}
}

// The job half (present defect 4's family): a parked run of a job that has
// gone is missed, although another job has its name. It was promoted on the
// source and the name, and the other job stood in for the one that had gone.
func TestAParkedJobRunIsOfItsOwnJob(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO jobs (uid, name, source, run_type, command, enabled, synced_at)
		VALUES ('j-other', 'nightly', 'cronomicon', 'bash', 'true', 1, 't')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	park := func(id, jobUID string) {
		t.Helper()
		params, _ := json.Marshal(EnqueueParams{JobName: "nightly", JobSource: "cronomicon", JobUID: jobUID,
			RunType: "bash", TriggeredBy: "someone", TriggerKind: "manual"})
		past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO pending_runs (id, kind, name, source, run_at, scheduled_by, created_at, status, params_json)
			VALUES (?, 'job', 'nightly', 'cronomicon', ?, 'someone', ?, 'pending', ?)`, id, past, past, string(params)); err != nil {
			t.Fatalf("park %s: %v", id, err)
		}
	}
	park("p-gone", "j-gone")
	park("p-live", "j-other")

	s := New(pool, quietLog(), nil)
	s.PromotePending(ctx)

	if st, reason := parkedState(t, pool, "p-gone"); st != "missed" || reason != "job no longer exists" {
		t.Errorf("the parked run of a job that no longer exists is %s (%q), want missed: another job has its name", st, reason)
	}
	if st, _ := parkedState(t, pool, "p-live"); st != "fired" {
		t.Errorf("the parked run of the job that is there is %s, want fired", st)
	}
	var n int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_name = 'nightly' AND status = 'queued'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d runs of nightly were queued (%v), want the one of the job that is there", n, err)
	}
}
