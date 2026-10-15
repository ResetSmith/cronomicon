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

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/runref"
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

	// The parent's home (GR-16): where a Git parent's sub-workflow is looked
	// up first, by the workflow the parent RUN was started for.
	var parentWorkflow int64
	_ = e.db.QueryRowContext(ctx, `SELECT COALESCE(workflow_id, 0) FROM workflow_runs WHERE id = ?`, parentTraceID).Scan(&parentWorkflow)
	src, steps, wfID, _, ok := e.loadChildWorkflow(ctx, WorkflowRef{Name: childName, UID: strings.TrimSpace(step.WorkflowUID)},
		parentSource, e.Home(ctx, parentWorkflow))
	if !ok {
		e.log.Error("workflow: sub-workflow not found", "parent", parentName, "child", childName)
		e.recordRefusedChild(ctx, parentTraceID, nodeID, childName, actor, parentSource, childDepth,
			"Refused: the referenced workflow does not exist or is disabled")
		return "danger", ""
	}
	// A workflow's sub-workflows are its agency's (§0a, IN-8). What spans at
	// an upgrade (a parent built in the app before §0a) fails this step here.
	if parentWorkflow != 0 {
		same, serr := e.sameAgency(ctx, parentWorkflow, wfID)
		if serr != nil {
			e.log.Error("workflow: compare the sub-workflow's agency with its parent's", "parent", parentName, "child", childName, "err", serr)
			return "danger", ""
		}
		if !same {
			e.log.Warn("workflow: sub-workflow is another agency's", "parent", parentName, "child", childName)
			e.recordRefusedChild(ctx, parentTraceID, nodeID, childName, actor, parentSource, childDepth, ReasonChildOtherAgency)
			return "danger", ""
		}
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

// ReasonChildOtherAgency is the stored reason of a sub-workflow step whose
// child is another agency's workflow.
const ReasonChildOtherAgency = "Refused: the referenced workflow is another agency's: a workflow runs its own agency's workflows only"

// sameAgency reports whether two workflows, by rowid, are one agency's (a Git
// workflow's repository's, the owner of one built in the app). One whose
// agency cannot be told (a Git workflow whose repository is gone) is no
// agency's, and matches nothing.
func (e *Engine) sameAgency(ctx context.Context, a, b int64) (bool, error) {
	var same bool
	err := e.db.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT `+runref.WorkflowAgencySQL+` FROM workflows w WHERE w.rowid = ?1)
		              = (SELECT `+runref.WorkflowAgencySQL+` FROM workflows w WHERE w.rowid = ?2), 0)`, a, b).Scan(&same)
	return same, err
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

// loadChildWorkflow resolves the referenced workflow. A binned or disabled
// workflow does not resolve — a sub-workflow step must not be a way to run
// something the catalog says is off — and, as for a job since FX-A1, the
// child a step names is not REPLACED by another of its name when it is off:
// the step fails (2.4.0, present defect 50; until then the lookup skipped the
// rows that were off, so binning one agency's child made its parent run
// ANOTHER agency's workflow of the name, on that agency's hosts).
//
// A step that pins its child (WorkflowRef.UID; every save of a workflow built
// in the app writes it) resolves by that identity alone.
//
// Otherwise, by the A11 source precedence: the parent's source first, then the
// other. home is the PARENT's (Engine.Home). For a Git parent whose home is
// known, a Git child is its own repository's workflow of the name, and no
// other's (§0a, IN-8; from GR-16 until then, Global's next). For a parent
// built in the app that pins nothing (one saved before 2.4.0): a child built
// in the app only when ONE workflow built in the app holds the name, as a
// step's job is refused when several do; and a Git child of Global's
// repository, the only one a parent of that age can have meant. childHome is
// where the child's own names are then looked up: a Git child's repository.
//
// Whatever is found here runs only when it is the parent's agency's: the
// caller refuses a child of another agency (§0a, IN-8).
func (e *Engine) loadChildWorkflow(ctx context.Context, ref WorkflowRef, parentSource, home string) (source string, steps []Step, wfID int64, childHome string, ok bool) {
	name := ref.Name
	if ref.UID != "" {
		var raw, src string
		var rowid int64
		var repo sql.NullString
		var off bool
		if err := e.db.QueryRowContext(ctx, `
			SELECT rowid, steps, source, repo_id, enabled = 0 OR deleted_at IS NOT NULL FROM workflows WHERE uid = ?`,
			ref.UID).Scan(&rowid, &raw, &src, &repo, &off); err != nil || off {
			return "", nil, 0, "", false
		}
		parsed, perr := ParseSteps(raw)
		if perr != nil {
			e.log.Error("workflow: sub-workflow has unparseable steps", "child", name, "uid", ref.UID, "err", perr)
			return "", nil, 0, "", false
		}
		if src == "git" {
			childHome = repoid.Global
			if repo.Valid && repo.String != "" {
				childHome = repo.String
			}
		}
		return src, parsed, rowid, childHome, true
	}
	order := []string{parentSource, "git"}
	if parentSource == "git" {
		order = []string{"git", "cronomicon"}
	}
	for _, src := range order {
		var raw string
		var rowid int64
		var repo sql.NullString
		err := sql.ErrNoRows
		switch agency := e.agencyOfHome(ctx, home); {
		case src == "git" && home != "":
			// The repository's own workflow of the name, and no other's (§0a,
			// IN-8; until then Global's next). One that may not run fails the
			// step, as a step whose job is disabled does.
			var off bool
			err = e.db.QueryRowContext(ctx, `
				SELECT rowid, steps, repo_id, enabled = 0 OR deleted_at IS NOT NULL FROM workflows
				 WHERE name = ? AND source = 'git' AND COALESCE(repo_id, ?) = ?`,
				name, repoid.Global, home).Scan(&rowid, &raw, &repo, &off)
			if err == nil && off {
				return "", nil, 0, "", false
			}
		case src == "cronomicon" && agency != "":
			// A workflow built in the app, for a parent of a repository:
			// that agency's, and no other's (see agencyOfHome).
			err = e.db.QueryRowContext(ctx, `
				SELECT rowid, steps, repo_id FROM workflows
				 WHERE name = ? AND source = 'cronomicon' AND enabled = 1 AND deleted_at IS NULL
				   AND owner_agency = ?
				 ORDER BY rowid LIMIT 1`,
				name, agency).Scan(&rowid, &raw, &repo)
		case src == "cronomicon":
			// Unpinned, among the workflows built in the app: the one that
			// holds the name, in any state. Several (two agencies' of one
			// name) is ambiguous and fails the step, as a step's job does; one
			// that is binned or disabled fails it too, and is not replaced.
			var n int
			var off bool
			if qerr := e.db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(MAX(enabled = 0 OR deleted_at IS NOT NULL), 0), COALESCE(MIN(rowid), 0)
				  FROM workflows WHERE name = ? AND source = 'cronomicon'`, name).Scan(&n, &off, &rowid); qerr != nil {
				err = qerr
				break
			}
			if n > 1 || (n == 1 && off) {
				return "", nil, 0, "", false
			}
			if n == 1 {
				err = e.db.QueryRowContext(ctx, `SELECT steps, repo_id FROM workflows WHERE rowid = ?`, rowid).Scan(&raw, &repo)
			}
		default:
			// Unpinned, among the Git workflows, for a parent with no home: Global's
			// repository's, the only one such a parent can have meant; and as
			// above, one that is off fails the step rather than being replaced.
			var off bool
			err = e.db.QueryRowContext(ctx, `
				SELECT rowid, steps, repo_id, enabled = 0 OR deleted_at IS NOT NULL FROM workflows
				 WHERE name = ? AND source = 'git' AND COALESCE(repo_id, ?) = ?`,
				name, repoid.Global, repoid.Global).Scan(&rowid, &raw, &repo, &off)
			if err == nil && off {
				return "", nil, 0, "", false
			}
		}
		if err != nil {
			continue
		}
		parsed, perr := ParseSteps(raw)
		if perr != nil {
			e.log.Error("workflow: sub-workflow has unparseable steps", "child", name, "source", src, "err", perr)
			continue
		}
		if src == "git" {
			childHome = repoid.Global
			if repo.Valid && repo.String != "" {
				childHome = repo.String
			}
		}
		return src, parsed, rowid, childHome, true
	}
	return "", nil, 0, "", false
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
			-- A child that was refused was not resolved to a row: the name's uid
			-- when one workflow holds it, and none when two do (present defect 34).
			(SELECT CASE WHEN COUNT(*) = 1 THEN MAX(uid) END FROM workflows WHERE name = ? AND source = ?))`,
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

// DescendantRunScopes returns the distinct scopes of the job runs that belong to
// the sub-workflow runs UNDER a workflow run — everything CancelTree would stop
// beyond the addressed run's own children (GC-10). An unbound run yields "".
func (e *Engine) DescendantRunScopes(ctx context.Context, wfTraceID string) ([]string, error) {
	pairs, err := e.DescendantRunAgencyScopes(ctx, wfTraceID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range pairs {
		if !seen[p.Scope] {
			seen[p.Scope] = true
			out = append(out, p.Scope)
		}
	}
	return out, nil
}

// DescendantRunAgencyScopes is DescendantRunScopes with each run's scope
// owner, the agency its scope is looked for in (I2), from the run's snapshot.
func (e *Engine) DescendantRunAgencyScopes(ctx context.Context, wfTraceID string) ([]AgencyScope, error) {
	rows, err := e.db.QueryContext(ctx, `
		WITH RECURSIVE tree(id) AS (
			SELECT id FROM workflow_runs WHERE parent_workflow_run_id = ?
			UNION
			SELECT w.id FROM workflow_runs w JOIN tree t ON w.parent_workflow_run_id = t.id
		)
		SELECT DISTINCT COALESCE(r.scope,''), `+agencyid.RunScopeOwnerSQL("r.agencies_json")+`
		  FROM runs r JOIN tree t ON r.workflow_run_id = t.id`,
		wfTraceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgencyScope
	for rows.Next() {
		var p AgencyScope
		if err := rows.Scan(&p.Scope, &p.Agency); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
