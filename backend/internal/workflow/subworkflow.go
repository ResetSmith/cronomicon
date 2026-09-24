package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// Sub-workflows: a workflow as a step of another workflow (SW,
// the prod-features plan §6).
//
// # What the child IS
//
// A real workflow run — its own workflow_runs row, its own walk, its own
// History entry — linked to the parent by parent_workflow_run_id and the
// parent step's node id. Deliberately not an inlined expansion of the child's
// steps into the parent's graph: an inlined child would lose its own identity,
// its own snapshot, and its own cancellability, and "which workflow failed"
// would stop having an answer.
//
// # The inheritance table (PF-Q12)
//
// Every run-context dimension either crosses the boundary or does not, and each
// choice is stated rather than left to fall out of the code:
//
//	env            EXPLICIT ONLY. The step's `inputs` map parent values into the
//	               child, exactly as they map into a job step. The parent's
//	               env_json does NOT flow implicitly — ambient inheritance
//	               across a reusable component is how a sub-workflow starts
//	               behaving differently depending on who called it.
//	scope          The child resolves its own, from its own steps' jobs. A
//	               workflow has no scope of its own to inherit.
//	actor          INHERITED. The person who triggered the parent is
//	               accountable for everything it caused; a child attributed to
//	               "system" would break that chain.
//	trigger kind   'workflow' on the child, which is what 950 added.
//	concurrency    The child's own steps' jobs apply their own policies. The
//	               parent holds NO key across the boundary — a parent that held
//	               one would serialize every workflow sharing a sub-workflow,
//	               which is the opposite of reuse.
//	calendar       NOT re-evaluated. CAL's veto is a SCHEDULE concept applied
//	               before a scheduled fire; a sub-workflow step is not a fire,
//	               it is the parent already running. Vetoing mid-descent would
//	               half-run the parent, which §2.6 already rejected for a
//	               workflow's own step jobs.
//	retries        The STEP's, applied to the child run as a unit: a failed
//	               child is re-triggered whole. Its internal steps' own retries
//	               have already been exhausted inside it.
//	cancel         PROPAGATES parent → child, recursively (see CancelTree).
//	reaction depth INHERITED unchanged, like a job step's — the hop was the
//	               reaction that started the parent.
//	workflow depth PARENT + 1, and never reset. The reaction-depth comment in
//	               engine.go records what resetting a depth counter on a child
//	               did last time: a cycle that ran forever.

// runWorkflowStep runs a child workflow as one step, applying the step's
// retries to the child run as a unit (the inheritance table's `retries` row).
//
// It mirrors runStep's shape deliberately: first attempt, then up to `retries`
// fresh child runs after the effective backoff, each its own workflow_runs row
// so every attempt stays visible in History. A workflow step has no jobDef to
// inherit from, so the values are the step's own.
func (e *Engine) runWorkflowStep(
	ctx context.Context,
	parentTraceID, parentName string,
	step Step,
	actor string,
	inputEnv map[string]string,
) (string, string, map[string]string) {
	retries, backoff, _ := effectiveRetry(step, jobDef{})
	status, childID := e.runChildWorkflowOnce(ctx, parentTraceID, parentName, step, actor, inputEnv)
	for attempt := 1; attempt <= retries && status == "danger"; attempt++ {
		if ctx.Err() != nil {
			break
		}
		if backoff > 0 {
			t := time.NewTimer(time.Duration(backoff) * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return status, childID, e.childOutputs(ctx, childID)
			case <-t.C:
			}
		}
		e.log.Info("workflow: retrying sub-workflow step",
			"parent", parentName, "child", step.Workflow, "attempt", attempt, "of", retries)
		status, childID = e.runChildWorkflowOnce(ctx, parentTraceID, parentName, step, actor, inputEnv)
	}
	return status, childID, e.childOutputs(ctx, childID)
}

// childOutputs re-exports a finished child run's outputs to the parent, so a
// sub-workflow step can feed {fromStep} inputs and output_match branches like
// any other step.
//
// The child's own step runs each capture their outputs; they are merged in
// completion order, so a later step's key wins over an earlier one's. That
// merge is the "declared names" rule in the simplest form the data supports:
// the child's outputs ARE its steps' outputs, addressed by the names the child
// already gave them. Nested grandchildren do not flow up — each boundary
// re-exports only what its own steps captured.
func (e *Engine) childOutputs(ctx context.Context, childTraceID string) map[string]string {
	if childTraceID == "" {
		return nil
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT outputs_json FROM runs
		 WHERE workflow_run_id = ? AND outputs_json IS NOT NULL AND outputs_json != ''
		 ORDER BY COALESCE(completed_at, created_at)`, childTraceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out map[string]string
	for rows.Next() {
		var js string
		if rows.Scan(&js) != nil {
			continue
		}
		m := map[string]string{}
		if json.Unmarshal([]byte(js), &m) != nil {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		maps.Copy(out, m)
	}
	return out
}

// runChildWorkflowOnce triggers a child workflow and waits for it, returning
// the attempt's terminal status and the child's trace id.
//
// The wait is DB-polled rather than in-memory: the child may be walked by a
// different engine instance (the API's and the scheduler's are separate
// objects), which is the same reason runCancelled reads a flag instead of a
// channel.
func (e *Engine) runChildWorkflowOnce(
	ctx context.Context,
	parentTraceID, parentName string,
	step Step,
	actor string,
	inputEnv map[string]string,
) (string, string) {
	childName := strings.TrimSpace(step.Workflow)
	nodeID := step.NodeID

	depth, reactionDepth, parentSource := e.parentContext(ctx, parentTraceID)
	childDepth := depth + 1
	if DepthExceeded(childDepth) {
		// The runtime backstop. Authoring-time cycle detection catches the
		// static case, but dual-source means the graph can change between
		// authoring and fire — and a cycle closed through a workflow edited
		// after its parent was validated reaches exactly here.
		e.log.Error("workflow: sub-workflow nesting ceiling reached; refusing to descend",
			"parent", parentName, "child", childName, "depth", childDepth, "max", MaxWorkflowDepth)
		e.recordRefusedChild(ctx, parentTraceID, nodeID, childName, actor, parentSource, childDepth,
			fmt.Sprintf("Refused: sub-workflow nesting would reach depth %d (max %d)", childDepth, MaxWorkflowDepth))
		return "danger", ""
	}

	src, steps, wfID, ok := e.loadChildWorkflow(ctx, childName, parentSource)
	if !ok {
		e.log.Error("workflow: sub-workflow not found", "parent", parentName, "child", childName)
		e.recordRefusedChild(ctx, parentTraceID, nodeID, childName, actor, parentSource, childDepth,
			"Refused: the referenced workflow does not exist or is disabled")
		return "danger", ""
	}

	// Only the step's explicit inputs cross the boundary — see the table above.
	var envJSON string
	if len(inputEnv) > 0 {
		if b, err := json.Marshal(inputEnv); err == nil {
			envJSON = string(b)
		}
	}

	res, err := e.Trigger(ctx, TriggerParams{
		WorkflowName:        childName,
		WorkflowSource:      src,
		WorkflowID:          wfID,
		Steps:               steps,
		TriggeredBy:         actor,
		TriggerKind:         "workflow",
		EnvJSON:             envJSON,
		ReactionDepth:       reactionDepth,
		ParentWorkflowRunID: parentTraceID,
		ParentNodeID:        nodeID,
		WorkflowDepth:       childDepth,
	})
	if err != nil {
		e.log.Error("workflow: trigger sub-workflow", "parent", parentName, "child", childName, "err", err)
		return "danger", ""
	}

	status := e.waitForWorkflowRun(ctx, res.TraceID)
	return status, res.TraceID
}

// parentContext reads the depths and source the child inherits from.
func (e *Engine) parentContext(ctx context.Context, parentTraceID string) (workflowDepth, reactionDepth int, source string) {
	var src sql.NullString
	_ = e.db.QueryRowContext(ctx,
		`SELECT COALESCE(workflow_depth,0), COALESCE(reaction_depth,0), workflow_source
		   FROM workflow_runs WHERE id = ?`, parentTraceID).Scan(&workflowDepth, &reactionDepth, &src)
	if src.String == "" {
		return workflowDepth, reactionDepth, "git"
	}
	return workflowDepth, reactionDepth, src.String
}

// loadChildWorkflow resolves the referenced workflow by the A11 source
// precedence: the parent's source first, then the other. A binned or disabled
// workflow does not resolve — a sub-workflow step must not be a way to run
// something the catalog says is off.
func (e *Engine) loadChildWorkflow(ctx context.Context, name, parentSource string) (source string, steps []Step, wfID int64, ok bool) {
	order := []string{parentSource, "git"}
	if parentSource == "git" {
		order = []string{"git", "amadeus"}
	}
	for _, src := range order {
		var raw string
		var rowid int64
		err := e.db.QueryRowContext(ctx, `
			SELECT rowid, steps FROM workflows
			 WHERE name = ? AND source = ? AND enabled = 1 AND deleted_at IS NULL`,
			name, src).Scan(&rowid, &raw)
		if err != nil {
			continue
		}
		parsed, perr := ParseSteps(raw)
		if perr != nil {
			e.log.Error("workflow: sub-workflow has unparseable steps", "child", name, "source", src, "err", perr)
			continue
		}
		return src, parsed, rowid, true
	}
	return "", nil, 0, false
}

// waitForWorkflowRun polls a child workflow run to terminal.
//
// The parent's ctx cancellation returns "danger" promptly, which is what makes
// a parent cancel stop waiting on a child it has just cancelled.
func (e *Engine) waitForWorkflowRun(ctx context.Context, traceID string) string {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "danger"
		case <-ticker.C:
			var status string
			var cancelled int
			err := e.db.QueryRowContext(ctx,
				`SELECT status, cancelled FROM workflow_runs WHERE id = ?`, traceID).Scan(&status, &cancelled)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "danger"
				}
				continue
			}
			if cancelled == 1 {
				return "danger"
			}
			switch status {
			case "success", "warning":
				return "success"
			case "failure", "killed":
				return "danger"
			case "skipped":
				return "skipped"
			}
		}
	}
}

// recordRefusedChild leaves a terminal workflow_runs row for a descent that was
// refused before it started.
//
// Without it a depth-ceiling refusal or a dangling reference would show as a
// parent that simply failed, with the reason living only in a log line — the
// invisible-failure shape this codebase keeps having to fix. The row is the
// child's, so History renders the refusal where the child would have been.
func (e *Engine) recordRefusedChild(ctx context.Context, parentTraceID, nodeID, childName, actor, source string, depth int, reason string) {
	if childName == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := e.db.ExecContext(ctx, `
		INSERT INTO workflow_runs
			(id, workflow_name, workflow_source, status, queued_reason, triggered_by, trigger_kind,
			 parent_workflow_run_id, parent_node_id, workflow_depth,
			 started_at, completed_at, created_at, workflow_uid)
		VALUES (?, ?, ?, 'skipped', ?, ?, 'workflow', ?, ?, ?, ?, ?, ?,
			(SELECT uid FROM workflows WHERE name = ? AND source = ?))`,
		nodeIDOrNew(nodeID), childName, source, reason, actor,
		nullStr(parentTraceID), nullStr(nodeID), depth, now, now, now,
		childName, source); err != nil {
		e.log.Error("workflow: record refused sub-workflow", "child", childName, "err", err)
	}
}

// CancelTree soft-cancels a workflow run and every descendant.
//
// Cancel propagation is depth-first over parent_workflow_run_id. It has to be
// explicit: a child is its own workflow run with its own walk goroutine, so
// cancelling the parent stops the parent's WAIT but would otherwise leave the
// child running to completion, doing work nobody is waiting for any more.
func (e *Engine) CancelTree(ctx context.Context, wfTraceID string) bool {
	cancelled := e.Cancel(ctx, wfTraceID)
	for _, child := range e.childWorkflowRuns(ctx, wfTraceID) {
		// Depth-first, and unconditional: a child may still be running even when
		// the parent has already finished (it should not be, but a crashed parent
		// leaves exactly that). The recursion terminates because
		// MaxWorkflowDepth bounds the tree.
		e.CancelTree(ctx, child)
	}
	return cancelled
}

// childWorkflowRuns returns the ids of a run's direct sub-workflow runs.
func (e *Engine) childWorkflowRuns(ctx context.Context, parentTraceID string) []string {
	rows, err := e.db.QueryContext(ctx,
		`SELECT id FROM workflow_runs WHERE parent_workflow_run_id = ?`, parentTraceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func nodeIDOrNew(nodeID string) string {
	if nodeID != "" {
		return nodeID
	}
	return db.NewTraceID()
}
