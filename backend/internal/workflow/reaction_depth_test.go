package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/reaction"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// RX — the depth ceiling's coverage of cycles that close through a WORKFLOW'S
// STEP GRAPH rather than through reaction edges (§2.8).
//
// §2.8 names the runtime ceiling as the backstop for exactly that shape,
// because static cycle detection cannot see it: the reaction graph is acyclic,
// and the loop only exists once you follow a workflow into the jobs it runs.
//
// The backstop had a hole. The engine builds its own INSERT for child steps
// rather than going through EnqueueRunWithID, and that column list omitted
// reaction_depth — so every child run started at the column default of 0. A
// chain routed reaction → workflow → its step → reaction therefore reset the
// counter every lap and never climbed. These tests are the fence.

func seedCascadeWorkflow(t *testing.T, pool *sql.DB, name, stepJob string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO workflows (uid, name, source, steps, enabled, synced_at)
		VALUES ('uid-wf-'||?, ?, 'git', ?, 1, '2026-01-01T00:00:00Z')`,
		name, name, `[{"type":"job","name":"`+stepJob+`"}]`); err != nil {
		t.Fatalf("seed workflow %q: %v", name, err)
	}
}

// primeReactor winds the reactor's cursor into the past. The first scan ever
// deliberately anchors and looks at nothing (a reaction is never retroactive),
// so a test that wants an event observed has to get past that.
func primeReactor(t *testing.T, pool *sql.DB) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO settings (key, value) VALUES ('reactionCursor', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("prime reactor cursor: %v", err)
	}
}

// finishQueuedRuns terminates every queued run in the database, standing in for
// the runner. Whole-DB rather than per-workflow because each lap of the cycle
// produces a NEW workflow run with a new trace id.
func finishQueuedRuns(pool *sql.DB) {
	_, _ = pool.Exec(
		`UPDATE runs SET status='success', completed_at=? WHERE status='queued'`,
		time.Now().UTC().Format(time.RFC3339))
}

// TestChildStepInheritsTheWorkflowRunsReactionDepth — the unit-level fact the
// cycle test depends on.
//
// The child inherits the parent's depth UNCHANGED rather than incremented: a
// workflow's internal steps are not reaction hops. The hop was the reaction
// that started the workflow; the steps are that one unit of work executing. A
// reaction watching the child then increments from there.
func TestChildStepInheritsTheWorkflowRunsReactionDepth(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "step-job")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "cascade", WorkflowSource: "git", WorkflowID: 1,
		TriggeredBy: "reactor", TriggerKind: "reaction",
		ReactionDepth: 2, ReactedToRunID: "r-upstream",
		Steps: []workflow.Step{{Type: "job", Name: "step-job"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, res.TraceID, func(string, int) string { return "success" })
	defer stop()
	waitWorkflowTerminal(t, pool, res.TraceID)

	var wfDepth, childDepth int
	if err := pool.QueryRow(`SELECT reaction_depth FROM workflow_runs WHERE id=?`, res.TraceID).Scan(&wfDepth); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(
		`SELECT reaction_depth FROM runs WHERE workflow_run_id=? AND job_name='step-job'`,
		res.TraceID).Scan(&childDepth); err != nil {
		t.Fatal(err)
	}
	if wfDepth != 2 {
		t.Errorf("workflow run depth = %d, want 2", wfDepth)
	}
	if childDepth != 2 {
		t.Errorf("child step depth = %d, want 2 — a step is not a reaction hop, so it carries the "+
			"parent's number; a child defaulting to 0 is what let a cycle through the step graph "+
			"reset the ceiling every lap", childDepth)
	}
}

// An ordinary (non-reaction) workflow run's children stay at 0, so nothing about
// this change puts a nonzero depth on runs that have nothing to do with
// reactions.
func TestOrdinaryWorkflowChildrenCarryNoReactionDepth(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "step-job")
	eng := workflow.New(pool, discardLog())

	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "plain", WorkflowSource: "git", WorkflowID: 1,
		TriggeredBy: "scheduler", TriggerKind: "scheduled",
		Steps: []workflow.Step{{Type: "job", Name: "step-job"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, res.TraceID, func(string, int) string { return "success" })
	defer stop()
	waitWorkflowTerminal(t, pool, res.TraceID)

	var childDepth int
	if err := pool.QueryRow(
		`SELECT reaction_depth FROM runs WHERE workflow_run_id=?`, res.TraceID).Scan(&childDepth); err != nil {
		t.Fatal(err)
	}
	if childDepth != 0 {
		t.Errorf("a scheduled workflow's child depth = %d, want 0", childDepth)
	}
}

// TestDepthCeilingStopsACycleThroughAWorkflowStepGraph — the real thing.
//
// The cycle: workflow `cascade` runs job `step-job`; a reaction watches
// `step-job` (opted in to workflow children) and runs `cascade`. The reaction
// graph itself is acyclic — one edge, job → workflow — so nothing static can
// refuse it. The loop only exists because the workflow runs the very job whose
// completion starts the workflow.
//
// This drives the REAL engine, the REAL reactor and the REAL promotion loop,
// wired the way main.go wires them, and asserts the chain terminates by itself.
func TestDepthCeilingStopsACycleThroughAWorkflowStepGraph(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)
	seedJob(t, pool, "step-job")
	seedCascadeWorkflow(t, pool, "cascade", "step-job")

	// The one reaction edge. include_workflow_children is what makes the cycle
	// reachable at all — without it a job running as a step emits nothing (§2.5).
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome,
		                       include_workflow_children, enabled, owner_uid)
		VALUES ('git','workflow','cascade','loop','git','job','step-job','any',1,1,
			(SELECT uid FROM workflows WHERE name='cascade' AND source='git'))`); err != nil {
		t.Fatalf("seed reaction: %v", err)
	}
	primeReactor(t, pool)

	eng := workflow.New(pool, discardLog())
	sched := scheduler.New(pool, discardLog(), nil)

	// The same wiring main.go installs: a promoted pending workflow run is
	// dispatched through the engine, carrying its trigger kind and its depth.
	trigger := func(depth int, cause string) {
		if _, err := eng.Trigger(ctx, workflow.TriggerParams{
			WorkflowName: "cascade", WorkflowSource: "git", WorkflowID: 1,
			TriggeredBy: "reactor", TriggerKind: "reaction",
			ReactionDepth: depth, ReactedToRunID: cause,
			Steps: []workflow.Step{{Type: "job", Name: "step-job"}},
		}); err != nil {
			t.Fatalf("trigger cascade: %v", err)
		}
	}
	sched.SetPendingWorkflowFirer(func(_ context.Context, p scheduler.PendingWorkflowFire) {
		trigger(p.ReactionDepth, p.ReactedToRunID)
	})

	// Lap 0: the reaction-fired workflow that starts the chain.
	trigger(1, "r-origin")

	// Turn the crank. The bound is generous and is a RUNAWAY DETECTOR, not the
	// expected number of laps: if the ceiling does not hold, this is what stops
	// the test instead of the test stopping the cycle.
	const maxLaps = 25
	laps := 0
	for ; laps < maxLaps; laps++ {
		finishQueuedRuns(pool)
		time.Sleep(60 * time.Millisecond) // let the walk observe its child
		finishQueuedRuns(pool)

		before := countWorkflowRuns(t, pool)
		sched.ScanReactions(ctx)
		sched.PromotePending(ctx)
		time.Sleep(60 * time.Millisecond)

		if countWorkflowRuns(t, pool) == before {
			break // the cascade stopped producing new runs
		}
	}

	if laps >= maxLaps {
		t.Fatalf("the cascade did not stop after %d laps — a cycle through a workflow's step graph "+
			"is exactly what the depth ceiling exists to catch, and a child run that starts at depth 0 "+
			"resets it every lap", maxLaps)
	}

	// It stopped for the RIGHT reason: the ceiling, recorded as such.
	var suppressed int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM reaction_deliveries WHERE result = 'suppressed_depth'`).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if suppressed == 0 {
		t.Error("the cascade stopped, but no delivery recorded suppressed_depth — it must stop " +
			"BECAUSE of the ceiling, not because the test's crank ran out")
	}

	// And it stopped where the ceiling says, not somewhere arbitrary. Each lap is
	// one reaction hop, so the deepest workflow run must sit just under MaxDepth.
	var deepest int
	if err := pool.QueryRow(`SELECT MAX(reaction_depth) FROM workflow_runs`).Scan(&deepest); err != nil {
		t.Fatal(err)
	}
	if deepest >= reaction.MaxDepth {
		t.Errorf("deepest workflow run = %d, which is at or past the ceiling of %d", deepest, reaction.MaxDepth)
	}
	if deepest < 2 {
		t.Errorf("deepest workflow run = %d — the chain should have climbed at least one hop before "+
			"being stopped, or this test is not exercising the ceiling at all", deepest)
	}

	// The ceiling hit also reached the Activity feed (§2.8/RX-Q6): it means a
	// cycle static detection missed, which is a defect rather than policy.
	var activity int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM activity WHERE category='Reactions' AND action='Depth ceiling'`).Scan(&activity); err != nil {
		t.Fatal(err)
	}
	if activity == 0 {
		t.Error("no Activity row for the ceiling hit — the delivery log is not somewhere a human looks")
	}
}

func countWorkflowRuns(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM workflow_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
