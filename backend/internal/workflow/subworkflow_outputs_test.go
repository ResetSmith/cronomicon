package workflow_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// SW × A12 — a sub-workflow step's outputs must cross the boundary.
//
// A12 made a step's captured outputs addressable downstream via
// {fromStep, fromOutput}; SW made a workflow a step. The two never met: the
// workflow step's JobResult carried a nil Outputs map, so resolveInputs took its
// "missing upstream" path and injected "" — deterministically, silently, with no
// error anywhere. A downstream job read an empty env var and did the wrong thing
// with it, which is the worst available failure mode. childOutputs closes it by
// re-exporting the child's own step runs' outputs.

// simulateAllRunsCapturing drains every queued run and lets the caller stamp the
// outputs and completion time of each. The completion time matters here and
// nowhere else: childOutputs merges the child's step outputs in COMPLETION
// order, so the timestamps ARE the collision rule under test.
func simulateAllRunsCapturing(pool *sql.DB, capture func(job string) (outputsJSON, completedAt string)) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				rows, err := pool.Query(`SELECT id, job_name FROM runs WHERE status='queued' ORDER BY created_at`)
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
					outputs, completedAt := capture(x.name)
					if completedAt == "" {
						completedAt = "2026-01-01T00:00:00Z"
					}
					_, _ = pool.Exec(
						`UPDATE runs SET status='success', completed_at=?, outputs_json=? WHERE id=?`,
						completedAt, outputs, x.id)
				}
			}
		}
	}()
	return func() { close(done) }
}

// runEnv reads a child run's persisted env snapshot as a map.
func runEnv(t *testing.T, pool *sql.DB, wfTraceID, jobName string) map[string]string {
	t.Helper()
	var js sql.NullString
	if err := pool.QueryRow(
		`SELECT env_json FROM runs WHERE workflow_run_id=? AND job_name=?`, wfTraceID, jobName).Scan(&js); err != nil {
		t.Fatalf("no run for %q: %v", jobName, err)
	}
	if !js.Valid || js.String == "" {
		t.Fatalf("run %q carries no env at all — its resolved inputs never reached the child run", jobName)
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(js.String), &m); err != nil {
		t.Fatalf("env_json for %q is not an object: %v", jobName, err)
	}
	return m
}

// TestSubWorkflowOutputs_ReachDownstreamInputs is the regression: a downstream
// job step's {fromStep: <workflow step>} input must carry the child's captured
// value, and a later child step must win a key collision.
func TestSubWorkflowOutputs_ReachDownstreamInputs(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "cap-early")
	seedJob(t, pool, "cap-late")
	seedJob(t, pool, "downstream")
	seedJob(t, pool, "gated")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"cap-early"},{"type":"job","name":"cap-late"}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "stage", Workflow: "child"},
		{Type: "job", Name: "downstream", Inputs: map[string]workflow.InputRef{
			"DEPLOY_ART":    {FromStep: "stage", FromOutput: "ARTIFACT"},
			"DEPLOY_REGION": {FromStep: "stage", FromOutput: "REGION"},
			"MISSING":       {FromStep: "stage", FromOutput: "nope"},
		}},
		// The other consumer of a step's Outputs: an output_match branch reading
		// a sub-workflow step's result exactly as it reads a job's.
		{Type: "branch",
			Condition: &workflow.Condition{Type: "output_match", JobRef: "stage",
				Field: "ARTIFACT", Operator: "==", Value: "app-9.9.9"},
			Pass: &workflow.Branch{Steps: []workflow.Step{{Type: "job", Name: "gated"}}},
			Fail: &workflow.Branch{Steps: []workflow.Step{}},
		},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllRunsCapturing(pool, func(job string) (string, string) {
		switch job {
		case "cap-early":
			return `{"ARTIFACT":"app-1.2.3","REGION":"eu-west-1"}`, "2026-01-01T00:00:01Z"
		case "cap-late":
			return `{"ARTIFACT":"app-9.9.9"}`, "2026-01-01T00:00:02Z"
		}
		return "", ""
	})
	defer stop()

	if status, _ := waitWorkflowTerminalWithin(t, pool, res.TraceID, 60*time.Second); status != "success" {
		t.Fatalf("parent status = %q, want success", status)
	}

	env := runEnv(t, pool, res.TraceID, "downstream")
	if env["DEPLOY_ART"] != "app-9.9.9" {
		t.Errorf("DEPLOY_ART = %q, want app-9.9.9 — \"\" means the workflow step's Outputs were nil; "+
			"app-1.2.3 means the merge is not in completion order", env["DEPLOY_ART"])
	}
	if env["DEPLOY_REGION"] != "eu-west-1" {
		t.Errorf("DEPLOY_REGION = %q, want eu-west-1 — a key only the EARLIER child step captured "+
			"must survive the merge, not be replaced wholesale", env["DEPLOY_REGION"])
	}
	// A key the child never captured still resolves deterministically to "" (A12).
	if v, ok := env["MISSING"]; !ok || v != "" {
		t.Errorf("MISSING = %q (present %v), want \"\"", v, ok)
	}

	// STATUS, not existence: the not-taken arm's jobs get a `skipped` run row of
	// their own (markSkipped), so counting rows would pass either way.
	var gated string
	_ = pool.QueryRow(
		`SELECT status FROM runs WHERE workflow_run_id=? AND job_name='gated'`, res.TraceID).Scan(&gated)
	if gated != "success" {
		t.Errorf("gated run status = %q, want success — the output_match pass arm did not run, "+
			"so the branch condition cannot see the sub-workflow's outputs", gated)
	}
}

// TestSubWorkflowOutputs_MergeInCompletionOrder pins the ordering rule against
// the shape that actually distinguishes it: two PARALLEL child steps, where the
// one that completes later is not the one inserted later. Ordering by insertion
// would give the opposite answer, and no serial child workflow could tell.
func TestSubWorkflowOutputs_MergeInCompletionOrder(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "arm-a")
	seedJob(t, pool, "arm-b")
	seedJob(t, pool, "downstream")
	seedWorkflow(t, pool, "child",
		`[{"type":"parallel","jobs":[{"type":"job","name":"arm-a"},{"type":"job","name":"arm-b"}]}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "stage", Workflow: "child"},
		{Type: "job", Name: "downstream", Inputs: map[string]workflow.InputRef{
			"WINNER": {FromStep: "stage", FromOutput: "WHO"},
		}},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// arm-a is dispatched first but completes LAST, so it wins the key.
	stop := simulateAllRunsCapturing(pool, func(job string) (string, string) {
		switch job {
		case "arm-a":
			return `{"WHO":"arm-a"}`, "2026-01-01T00:00:09Z"
		case "arm-b":
			return `{"WHO":"arm-b"}`, "2026-01-01T00:00:02Z"
		}
		return "", ""
	})
	defer stop()

	if status, _ := waitWorkflowTerminalWithin(t, pool, res.TraceID, 60*time.Second); status != "success" {
		t.Fatalf("parent status = %q, want success", status)
	}
	if env := runEnv(t, pool, res.TraceID, "downstream"); env["WINNER"] != "arm-a" {
		t.Errorf("WINNER = %q, want arm-a (the later-COMPLETING child step)", env["WINNER"])
	}
}

// TestSubWorkflowOutputs_ChildWithNoneIsNotAnError: a child whose steps capture
// nothing must leave the step's inputs resolving to "" as A12 specifies, not
// fail the parent or panic on a nil map.
func TestSubWorkflowOutputs_ChildWithNoneIsNotAnError(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "quiet")
	seedJob(t, pool, "downstream")
	seedWorkflow(t, pool, "child", `[{"type":"job","name":"quiet"}]`)

	steps := []workflow.Step{
		{Type: "workflow", Name: "stage", Workflow: "child"},
		{Type: "job", Name: "downstream", Inputs: map[string]workflow.InputRef{
			"NOTHING": {FromStep: "stage", FromOutput: "ANY"},
		}},
	}
	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateAllRunsCapturing(pool, func(string) (string, string) { return "", "" })
	defer stop()

	if status, _ := waitWorkflowTerminalWithin(t, pool, res.TraceID, 60*time.Second); status != "success" {
		t.Fatalf("parent status = %q, want success", status)
	}
	if v := runEnv(t, pool, res.TraceID, "downstream")["NOTHING"]; v != "" {
		t.Errorf("NOTHING = %q, want \"\"", v)
	}
}
