package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// SW — a workflow as a step of another workflow.
//
// The two failures worth defending against are a child that runs when it should
// not have (a cycle, or a graph nested past the ceiling) and a child that keeps
// running after its parent is cancelled. Both are silent: the first burns
// forever, the second does work nobody is waiting for.

func seedWorkflow(t *testing.T, pool *sql.DB, name, steps string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO workflows (name, source, steps, enabled, synced_at) VALUES (?, 'git', ?, 1, 't')`,
		name, steps); err != nil {
		t.Fatalf("seed workflow %s: %v", name, err)
	}
}

// simulateAllRuns drains every queued run in the database, whichever workflow
// run owns it. simulateRuns filters by workflow_run_id, which is exactly wrong
// here: a sub-workflow's jobs belong to the CHILD's run, so a parent-scoped
// drainer would leave them queued and the parent would wait forever.
func simulateAllRuns(pool *sql.DB, outcome func(job string) string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				rows, err := pool.Query(`SELECT id, job_name FROM runs WHERE status='queued'`)
				if err != nil {
					continue
				}
				type q struct{ id, name string }
				var qs []q
				for rows.Next() {
					var x q
					if rows.Scan(&x.id, &x.name) == nil {
						qs = append(qs, x)
					}
				}
				rows.Close()
				for _, x := range qs {
					_, _ = pool.Exec(`UPDATE runs SET status=?, completed_at=? WHERE id=?`,
						outcome(x.name), "2026-01-01T00:00:00Z", x.id)
				}
			}
		}
	}()
	return func() { close(done) }
}

func wfRunStatus(t *testing.T, pool *sql.DB, traceID string) (status string, cancelled int) {
	t.Helper()
	_ = pool.QueryRow(`SELECT status, cancelled FROM workflow_runs WHERE id=?`, traceID).Scan(&status, &cancelled)
	return
}

// The happy path: a parent runs a child as one step and both complete.
func TestSubWorkflowRunsAsAStep(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedJob(t, pool, "after")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "run-child", Workflow: "child"},
		{Type: "job", Name: "after"},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "ada@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllRuns(pool, func(string) string { return "success" })
	defer stop()

	status, _ := waitWorkflowTerminal(t, pool, result.TraceID)
	if status != "success" {
		t.Fatalf("parent status = %q, want success", status)
	}

	// The child is a REAL workflow run, linked and attributed.
	var childID, childName, triggerKind, actor string
	var parentID sql.NullString
	var depth int
	if err := pool.QueryRow(`
		SELECT id, workflow_name, trigger_kind, triggered_by, parent_workflow_run_id, workflow_depth
		  FROM workflow_runs WHERE workflow_name='child'`).
		Scan(&childID, &childName, &triggerKind, &actor, &parentID, &depth); err != nil {
		t.Fatalf("no child workflow run was created: %v", err)
	}
	if triggerKind != "workflow" {
		t.Errorf("child trigger_kind = %q, want workflow", triggerKind)
	}
	if !parentID.Valid || parentID.String != result.TraceID {
		t.Errorf("child parent link = %v, want %s", parentID, result.TraceID)
	}
	if depth != 1 {
		t.Errorf("child workflow_depth = %d, want 1", depth)
	}
	// The actor crosses the boundary: whoever triggered the parent is
	// accountable for everything it caused.
	if actor != "ada@example.com" {
		t.Errorf("child actor = %q, want the parent's — a child attributed elsewhere breaks the chain", actor)
	}
	// The child's own job ran.
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='child-job'`).Scan(&n)
	if n == 0 {
		t.Error("the child workflow's job never ran")
	}
}

// A parent whose child fails fails, unless the step says continue.
func TestSubWorkflowFailurePropagates(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "workflow", Name: "run-child", Workflow: "child"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllRuns(pool, func(string) string { return "failure" })
	defer stop()

	status, _ := waitWorkflowTerminal(t, pool, result.TraceID)
	if status == "success" {
		t.Error("the parent succeeded despite its sub-workflow failing")
	}
}

// A reference to a workflow that does not exist fails the step AND leaves a row
// saying why — not a parent that merely failed with the reason in a log line.
func TestSubWorkflowMissingReferenceIsRecorded(t *testing.T) {
	pool := openPool(t)

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "workflow", Name: "run-child", Workflow: "nope"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, _ := waitWorkflowTerminal(t, pool, result.TraceID)
	if status == "success" {
		t.Error("a dangling sub-workflow reference succeeded")
	}
	var reason sql.NullString
	if err := pool.QueryRow(
		`SELECT queued_reason FROM workflow_runs WHERE workflow_name='nope' AND status='skipped'`).Scan(&reason); err != nil {
		t.Fatalf("no row records the refused descent: %v", err)
	}
	if reason.String == "" {
		t.Error("the refusal row carries no reason")
	}
}

// The runtime depth ceiling. Authoring-time cycle detection cannot be the only
// guard: dual-source means the graph can change between authoring and fire.
func TestSubWorkflowDepthCeilingRefusesToDescend(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "leaf")
	// A self-referencing chain the compose validator would have rejected, but
	// which nothing stops arriving via Git.
	seedWorkflow(t, pool, "loop", `[{"type":"workflow","name":"again","workflow":"loop"}]`)

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "loop", WorkflowSource: "git", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "workflow", Name: "again", Workflow: "loop"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// The point is that it TERMINATES at all; the status itself is not asserted.
	_, _ = waitWorkflowTerminal(t, pool, result.TraceID)

	// Measured over runs that actually RAN: the refusal row is deliberately
	// stamped at the depth it would have reached, which is informative but is not
	// nesting that happened.
	var deepest int
	_ = pool.QueryRow(
		`SELECT COALESCE(MAX(workflow_depth),0) FROM workflow_runs WHERE status != 'skipped'`).Scan(&deepest)
	if deepest > workflow.MaxWorkflowDepth {
		t.Errorf("nesting reached depth %d, past the ceiling of %d — the recursion is unbounded",
			deepest, workflow.MaxWorkflowDepth)
	}
	var refused int
	_ = pool.QueryRow(
		`SELECT COUNT(*) FROM workflow_runs WHERE status='skipped' AND queued_reason LIKE 'Refused%'`).Scan(&refused)
	if refused == 0 {
		t.Error("the ceiling stopped the descent but recorded nothing")
	}
}

// Cancelling a parent must stop its children too.
func TestCancelTreePropagatesToChildren(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	eng := workflow.New(pool, discardLog())

	// Build the tree by hand: the engine's own linkage is covered above, and this
	// isolates propagation from timing.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	      VALUES('p','parent','running','t','manual','2026-08-11T00:00:00Z')`)
	exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at,
	                                parent_workflow_run_id, workflow_depth)
	      VALUES('c1','child','running','t','workflow','2026-08-11T00:00:00Z','p',1)`)
	exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at,
	                                parent_workflow_run_id, workflow_depth)
	      VALUES('gc','grandchild','running','t','workflow','2026-08-11T00:00:00Z','c1',2)`)

	if !eng.CancelTree(ctx, "p") {
		t.Fatal("CancelTree reported nothing cancelled")
	}
	for _, id := range []string{"p", "c1", "gc"} {
		_, cancelled := wfRunStatus(t, pool, id)
		if cancelled != 1 {
			t.Errorf("%s was not cancelled — a child left running does work nobody is waiting for", id)
		}
	}
}

// A workflow step's result is addressable by later steps exactly as a job's is:
// that indistinguishability is what makes it a reusable component.
func TestSubWorkflowResultIsAddressableDownstream(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "child-job")
	seedJob(t, pool, "downstream")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"child-job"}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "stage-one", Workflow: "child"},
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "stage-one"},
			Pass:      &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "downstream"}}},
			Fail:      &workflow.Branch{Steps: []workflow.Step{}},
		},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("a branch condition on a workflow step should validate: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllRuns(pool, func(string) string { return "success" })
	defer stop()

	if status, _ := waitWorkflowTerminal(t, pool, result.TraceID); status != "success" {
		t.Fatalf("parent status = %q, want success", status)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='downstream'`).Scan(&n)
	if n == 0 {
		t.Error("the pass arm did not run — a workflow step's result is not reaching the branch condition")
	}
}

// Structural validation of the new step type.
func TestWorkflowStepValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []workflow.Step
		want  bool // want errors
	}{
		{"valid", []workflow.Step{{Type: "workflow", Name: "a", Workflow: "child"}}, false},
		{"no name", []workflow.Step{{Type: "workflow", Workflow: "child"}}, true},
		{"no workflow", []workflow.Step{{Type: "workflow", Name: "a"}}, true},
		{"carries jobs[]", []workflow.Step{{Type: "workflow", Name: "a", Workflow: "c",
			Jobs: []workflow.Step{{Type: "job", Name: "x"}}}}, true},
		{"as a bare parallel arm", []workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
			{Type: "workflow", Name: "a", Workflow: "c"}}}}, true},
		{"wrapped in a sequence arm", []workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
			{Type: "sequence", Steps: []workflow.Step{{Type: "workflow", Name: "a", Workflow: "c"}}}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := workflow.ValidateSteps(tc.steps)
			if got := len(errs) > 0; got != tc.want {
				t.Errorf("errors = %v (%+v), want %v", got, errs, tc.want)
			}
		})
	}
}
