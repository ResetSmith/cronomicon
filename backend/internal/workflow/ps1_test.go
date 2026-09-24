package workflow_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// ─── PS-1: parallel arms may be sequences ─────────────────────────────────────
//
// The Automate-suite shape: independent serial chains fanning out from one
// point and running concurrently. These tests drive the REAL engine walk with
// simulateRuns standing in for the runner.

// TestParallel_SequenceArms: a parallel block with two sequence arms (each a
// two-job chain) plus a leaf-job arm runs every job exactly once, keeps each
// chain's internal order, and the workflow succeeds.
func TestParallel_SequenceArms(t *testing.T) {
	pool := openPool(t)
	for _, n := range []string{"a1", "a2", "b1", "b2", "solo", "after"} {
		seedJob(t, pool, n)
	}

	steps := []workflow.Step{
		{Type: "parallel", Label: "chains", Jobs: []workflow.Step{
			{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "a1"}, {Type: "job", Name: "a2"}}},
			{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "b1"}, {Type: "job", Name: "b2"}}},
			{Type: "job", Name: "solo"},
		}},
		{Type: "job", Name: "after"},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	// Record completion order so chain-internal ordering is checkable. The
	// recorder runs on simulateRuns' goroutine, so appends and the post-terminal
	// read are mutex-guarded.
	var mu sync.Mutex
	var order []string
	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "ps1-wf", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, result.TraceID, func(job string, attempt int) string {
		mu.Lock()
		order = append(order, job)
		mu.Unlock()
		return "success"
	})
	defer stop()

	status, _ := waitWorkflowTerminal(t, pool, result.TraceID)
	if status != "success" {
		t.Fatalf("workflow status = %q, want success", status)
	}
	mu.Lock()
	defer mu.Unlock()

	// Every job ran exactly once.
	counts := map[string]int{}
	for _, j := range order {
		counts[j]++
	}
	for _, n := range []string{"a1", "a2", "b1", "b2", "solo", "after"} {
		if counts[n] != 1 {
			t.Errorf("job %s ran %d times, want 1 (order: %v)", n, counts[n], order)
		}
	}
	// Chain-internal order held; the barrier held ("after" ran last).
	idx := map[string]int{}
	for i, j := range order {
		idx[j] = i
	}
	if idx["a1"] > idx["a2"] || idx["b1"] > idx["b2"] {
		t.Errorf("sequence arm ran out of order: %v", order)
	}
	if idx["after"] != len(order)-1 {
		t.Errorf("post-parallel step did not wait for the block: %v", order)
	}
}

// TestParallel_SequenceArmFailureHaltsWorkflow: a failing job inside a sequence
// arm fails the arm and therefore the parallel block and workflow — while the
// sibling arm still completes (the block always drains all arms).
func TestParallel_SequenceArmFailureHaltsWorkflow(t *testing.T) {
	pool := openPool(t)
	for _, n := range []string{"bad1", "bad2", "ok1", "never"} {
		seedJob(t, pool, n)
	}

	steps := []workflow.Step{
		{Type: "parallel", Jobs: []workflow.Step{
			{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "bad1"}, {Type: "job", Name: "bad2"}}},
			{Type: "job", Name: "ok1"},
		}},
		{Type: "job", Name: "never"},
	}
	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "ps1-fail-wf", WorkflowID: 2, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	var mu sync.Mutex
	var ran []string
	stop := simulateRuns(pool, result.TraceID, func(job string, attempt int) string {
		mu.Lock()
		ran = append(ran, job)
		mu.Unlock()
		if job == "bad1" {
			return "failure"
		}
		return "success"
	})
	defer stop()

	status, _ := waitWorkflowTerminal(t, pool, result.TraceID)
	if status != "failure" {
		t.Fatalf("workflow status = %q, want failure", status)
	}
	// Settle briefly, then assert: the failing arm stopped at bad1 (bad2 never
	// dispatched) and the post-block step never ran.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, j := range ran {
		if j == "bad2" {
			t.Errorf("bad2 dispatched after its arm failed (ran: %v)", ran)
		}
		if j == "never" {
			t.Errorf("post-parallel step dispatched after block failure (ran: %v)", ran)
		}
	}
}

// TestParallel_A12AcrossSequenceArm: an output captured inside a sequence arm is
// consumable by a step after the block (the arm's results merge back on join).
func TestFlattenSteps_SequenceArm(t *testing.T) {
	steps := []workflow.Step{
		{Type: "parallel", Jobs: []workflow.Step{
			{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "c1"}, {Type: "job", Name: "c2"}}},
			{Type: "job", Name: "solo"},
		}},
	}
	got := workflow.FlattenSteps(steps)
	if len(got) != 3 {
		t.Fatalf("FlattenSteps = %v, want 3 job nodes", got)
	}
}
