// Package workflow implements the workflow execution engine.
//
// A workflow run is created by inserting a workflow_runs row, then walking the
// step graph (flattenSteps / evaluateCondition / branching from the prototype).
// Each step becomes a child runs row with workflow_run_id set (S3 FK).  B4
// transitions child runs queued→running→terminal; the engine monitors child
// run status to advance branches and compute workflow completion.
//
// Workflow-start is emitted at trigger time; workflow-end is emitted when all
// non-skipped child runs reach a terminal status.  B5 owns these emissions;
// B4 owns run-start and run-end for normally-dispatched runs.
package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/envmerge"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// ─── Step types (mirrors prototype's workflow steps) ─────────────────────────

// Step is one element in a workflow's steps array.
type Step struct {
	Type   string `json:"type"` // job | parallel | branch | sequence | workflow
	Name   string `json:"name,omitempty"`
	Label  string `json:"label,omitempty"`
	Source string `json:"jobSource,omitempty"` // A11 per-step override (git|amadeus); empty ⇒ workflow source then fallback
	// JobUID pins the step to ONE job by its permanent identity (R2F-2). Since
	// R2-5 a name may belong to two departments' jobs, and a name-only step whose
	// name is ambiguous REFUSES at run time — correct, but it makes a legal
	// catalog state unbuildable. Setting this resolves the step in one indexed
	// lookup, with no A11 source-precedence walk: an explicit identity needs no
	// fallback, and a dangling one refuses rather than falling back to a
	// same-named job (the RX purge-recreate semantics — a new identity is a new
	// job on purpose).
	//
	// Name and JobUID travel TOGETHER: the name stays the human-readable identity
	// everywhere (History, the results map, {fromStep}, YAML round-trips) and the
	// uid is what resolves. Compose refuses a pair that disagrees, or the display
	// would lie about what runs. Empty is the norm: every graph authored before
	// this band, and every git-synced graph, is name-only forever (names are the
	// law in git — see the yaml validator's warning).
	JobUID    string              `json:"jobUid,omitempty"`
	Inputs    map[string]InputRef `json:"inputs,omitempty"` // A12: env KEY ← an upstream step's captured output
	Jobs      []Step              `json:"jobs,omitempty"`   // present when type=parallel: the concurrent arms (leaf jobs or sequences, PS-1)
	Steps     []Step              `json:"steps,omitempty"`  // present when type=sequence: an ordered serial chain (PS-1)
	Pass      *Branch             `json:"pass,omitempty"`
	Fail      *Branch             `json:"fail,omitempty"`
	Condition *Condition          `json:"condition,omitempty"`

	// WB-R1 per-step retry config. Each is a pointer so "unset" (nil) falls back to
	// the job-definition default, while an explicit 0/false overrides it (D3 — step
	// value wins). Retries: re-dispatch a fresh child run on failure up to N times;
	// BackoffSeconds: wait between attempts; ContinueOnError: skip-and-proceed
	// instead of halting when a step still fails after its retries.
	Retries         *int  `json:"retries,omitempty"`
	BackoffSeconds  *int  `json:"backoffSeconds,omitempty"`
	ContinueOnError *bool `json:"continueOnError,omitempty"`

	// Workflow is the referenced workflow's name, present when type=workflow (SW).
	// It is a SEPARATE field from Name rather than reusing it, because a workflow
	// step needs both: Name is its position in the parent's NAME-keyed results
	// map (what {fromStep} addresses), and this is what it runs. Collapsing them
	// would forbid two steps running the same sub-workflow with different inputs.
	Workflow string `json:"workflow,omitempty"`

	// NodeID is the per-NODE child-run trace ID (runs.id PK), minted by
	// assignNodeIDs before the walk. Keying run identity by node position rather
	// than job name lets the same job name appear in multiple steps without a PK
	// collision, and gives nested-branch jobs a non-empty id (PP-H8 a/c). Empty on
	// a freshly-parsed definition (omitempty keeps it out of definition reads); the
	// per-run snapshot (WB-D1) serializes it so a run's child runs map back to their
	// graph nodes for grouped rendering (WB-O3).
	NodeID string `json:"id,omitempty"`
}

// InputRef points a step's injected env var at an upstream step's output (A12 /
// Q-F: explicit {fromStep,fromOutput} — never a flat global merge).
type InputRef struct {
	FromStep   string `json:"fromStep"`
	FromOutput string `json:"fromOutput"`
}

// Branch holds a set of steps for the taken/not-taken arm of a branch step.
type Branch struct {
	Steps []Step `json:"steps"`
}

// Condition is the predicate on a branch step.
type Condition struct {
	Type     string `json:"type"` // job_status | output_match
	JobRef   string `json:"jobRef"`
	Field    string `json:"field,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
}

// JobResult carries the terminal outcome of a single job in a workflow run.
type JobResult struct {
	Status  string // success | danger | skipped
	Outputs map[string]string
	TraceID string
}

// ─── Engine ──────────────────────────────────────────────────────────────────

// Engine drives workflow execution.
type Engine struct {
	db  *sql.DB
	log *slog.Logger

	// WB-S2: trace id → walk cancel func, for soft-cancelling an in-flight run.
	// Guarded by mu; entries are added at Trigger and removed when the walk ends.
	mu         sync.Mutex
	cancels    map[string]context.CancelFunc
	shutdownWG *sync.WaitGroup
}

// New creates an Engine.
func New(database *sql.DB, log *slog.Logger) *Engine {
	return &Engine{db: database, log: log, cancels: map[string]context.CancelFunc{}}
}

// TriggerParams holds the inputs for starting a workflow run.
type TriggerParams struct {
	WorkflowName   string
	WorkflowSource string // git | amadeus (A9); empty ⇒ 'git'. Default source for step job resolution (A11).
	WorkflowID     int64
	Steps          []Step
	Scope          string
	TriggeredBy    string // user email
	TriggerKind    string // manual | scheduled | reaction (empty ⇒ "manual")
	ScheduleName   string // schedule entry that fired (empty ⇒ NULL)
	EnvJSON        string // env snapshot as a JSON string (empty ⇒ NULL); propagated to child runs
	// RX — reaction provenance, set only when a reactor produced this run.
	// ReactionDepth is the chain position (upstream + 1) and is what the depth
	// ceiling reads on the NEXT hop; ReactedToRunID is the durable because-of
	// link. Both live on workflow_runs for the same reason they live on runs:
	// the delivery log that also knows them is retention-pruned.
	ReactionDepth  int
	ReactedToRunID string
	// SW — sub-workflow provenance, set only when a parent workflow step produced
	// this run. WorkflowDepth is INHERITED-PLUS-ONE by the caller, never reset
	// here: engine.go's reaction-depth comment records what resetting a depth
	// counter on the child did last time (a cycle that ran forever).
	ParentWorkflowRunID string
	ParentNodeID        string
	WorkflowDepth       int
}

// TriggerResult is returned by Trigger.
type TriggerResult struct {
	TraceID     string
	JobTraceIDs []string
}

// Trigger starts a workflow run:
//  1. Inserts a workflow_runs row.
//  2. Flattens the step graph and mints trace IDs for all jobs.
//  3. Enqueues the first eligible job(s) as runs rows.
//  4. Emits workflow-start activity.
//  5. Launches a monitor goroutine that advances the workflow as child runs complete.
func (e *Engine) Trigger(ctx context.Context, p TriggerParams) (*TriggerResult, error) {
	wfTraceID := db.NewTraceID()
	now := time.Now().UTC().Format(time.RFC3339)
	wfSource := p.WorkflowSource
	if wfSource == "" {
		wfSource = "git"
	}

	// Mint one trace ID per JOB NODE (per step position, recursing into nested
	// branches + parallel) so duplicate job names and nested-branch jobs each get
	// a unique runs.id PK (PP-H8 a/c). The results map below stays NAME-keyed —
	// branch conditions and A12 inputs address upstream steps by name.
	assignNodeIDs(p.Steps)
	nodes := flattenJobNodes(p.Steps)
	jobTraceIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		jobTraceIDs = append(jobTraceIDs, n.NodeID)
	}

	// WB-D1 — capture the step graph as it ran (with the node IDs just minted) so
	// History can render this run's structure even after the definition is edited,
	// and the run-detail serializer can map child runs back to their graph nodes for
	// grouped parallel/branch rendering (WB-O3). steps_hash is a cheap change-detector.
	snapshot, snapshotHash := snapshotSteps(p.Steps)

	// Insert workflow_runs. trigger_kind defaults to "manual" (HTTP path); the
	// scheduler passes "scheduled" plus the entry name and env snapshot.
	triggerKind := p.TriggerKind
	if triggerKind == "" {
		triggerKind = "manual"
	}
	_, err := e.db.ExecContext(ctx, `
		INSERT INTO workflow_runs
			(id, workflow_id, workflow_name, workflow_source, status, triggered_by, trigger_kind,
			 schedule_name, env_json, steps_snapshot, steps_hash,
			 reaction_depth, reacted_to_run_id,
			 parent_workflow_run_id, parent_node_id, workflow_depth,
			 started_at, created_at, workflow_uid)
		VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			(SELECT uid FROM workflows WHERE name = ? AND source = ?))
	`, wfTraceID, p.WorkflowID, p.WorkflowName, wfSource, p.TriggeredBy, triggerKind,
		nullStr(p.ScheduleName), nullStr(p.EnvJSON), nullStr(snapshot), nullStr(snapshotHash),
		p.ReactionDepth, nullStr(p.ReactedToRunID),
		nullStr(p.ParentWorkflowRunID), nullStr(p.ParentNodeID), p.WorkflowDepth,
		now, now,
		p.WorkflowName, wfSource)
	if err != nil {
		return nil, fmt.Errorf("insert workflow_run: %w", err)
	}

	// Emit workflow-start activity.
	if err := EmitActivity(ctx, e.db, ActivityParams{
		Kind:         "workflow-start",
		Actor:        p.TriggeredBy,
		WorkflowName: p.WorkflowName,
		WorkflowID:   p.WorkflowID,
		TraceID:      wfTraceID,
		Scope:        p.Scope,
	}); err != nil {
		e.log.Error("workflow: emit workflow-start", "err", err)
	}

	// Look up job definitions for run_type/scope/source via the A11 precedence
	// (step.Source > workflow source > fallback).
	jobDefs, err := e.lookupJobDefs(ctx, p.Steps, wfSource)
	if err != nil {
		return nil, fmt.Errorf("look up job defs: %w", err)
	}

	// Start the walk in a goroutine — the trigger returns immediately. The walk
	// runs on a cancellable context (not context.Background()) so the cancel
	// endpoint (WB-S2) can stop in-flight waits and further dispatch; the cancel
	// func is registered for the run's lifetime and cleaned up when it ends.
	go func() {
		bgCtx, cancel := context.WithCancel(context.Background())
		e.registerCancel(wfTraceID, cancel)
		defer e.unregisterCancel(wfTraceID)
		defer cancel()
		e.runWorkflow(bgCtx, wfTraceID, p.WorkflowName, p.WorkflowID, p.Steps, p.Scope, p.TriggeredBy, jobDefs)
	}()

	return &TriggerResult{
		TraceID:     wfTraceID,
		JobTraceIDs: jobTraceIDs,
	}, nil
}

// ─── Step walking ─────────────────────────────────────────────────────────────

// runWorkflow walks the step graph and finalizes the workflow EXACTLY ONCE
// (PP-H8 b). The inner walkSteps never finalizes — it only returns a status — so
// a branch arm completing can't emit a second workflow-end, and a branch that
// isn't the last step doesn't terminate the workflow before later steps run.
func (e *Engine) runWorkflow(
	ctx context.Context,
	wfTraceID, wfName string,
	wfID int64,
	steps []Step,
	scope, actor string,
	jobDefs map[string]jobDef,
) {
	status := e.walkSteps(ctx, wfTraceID, wfName, wfID, steps, scope, actor, jobDefs, map[string]*JobResult{})
	final := "success"
	if status == "danger" {
		final = "danger"
	}
	// Finalize on a detached context: a soft-cancel (WB-S2) cancels the walk ctx, so
	// finishWorkflow's terminal write must not ride that cancelled context or it
	// would fail and leave the run stuck 'running'.
	e.finishWorkflow(context.WithoutCancel(ctx), wfTraceID, wfName, wfID, final, actor)
}

// walkSteps recursively walks the step graph, enqueuing jobs and waiting for
// completion before advancing. It returns "success" (all steps ran) or "danger"
// (a non-branch-ref job failed → halt). It does NOT finalize the workflow — that
// is runWorkflow's job, called once at the top level (PP-H8 b).
func (e *Engine) walkSteps(
	ctx context.Context,
	wfTraceID, wfName string,
	wfID int64,
	steps []Step,
	scope, actor string,
	jobDefs map[string]jobDef,
	results map[string]*JobResult,
) string {
	// Collect branch refs for "do not halt on failure" logic.
	branchRefs := collectBranchRefs(steps)

	for _, step := range steps {
		// WB-S2: stop dispatching further steps once the run is cancelled — via the
		// local walk ctx (fast path for the engine that holds the cancel func) OR the
		// persisted flag (so cancel also works for runs driven by another engine
		// instance, e.g. the scheduler's per-trigger engine). The ctx.Err() short-
		// circuit avoids querying on an already-cancelled context.
		if ctx.Err() != nil || e.runCancelled(ctx, wfTraceID) {
			return "danger"
		}
		switch stepKind(step) {
		case "job":
			inputEnv := e.resolveInputs(step, results) // A12: upstream outputs → injected env
			status, traceID := e.runStep(ctx, wfTraceID, wfName, wfID, step, scope, actor, jobDefs, inputEnv)
			results[step.Name] = &JobResult{Status: status, TraceID: traceID, Outputs: e.loadOutputs(ctx, traceID)}
			// WB-R1: a still-failing step halts the walk unless it is a branch ref
			// or its effective continue-on-error is set.
			if status == "danger" && !branchRefs[step.Name] && !effectiveContinueOnError(step, jobDefs[StepKey(step)]) {
				return "danger"
			}

		case stepKindWorkflow:
			// SW: run another workflow as a unit and wait for it. The result
			// enters the NAME-keyed results map exactly as a job step's does, so
			// {fromStep} and branch conditions address it the same way — the
			// difference is invisible to everything downstream, which is the
			// point of a reusable component.
			inputEnv := e.resolveInputs(step, results)
			status, childID, outputs := e.runWorkflowStep(ctx, wfTraceID, wfName, step, actor, inputEnv)
			results[step.Name] = &JobResult{Status: status, TraceID: childID, Outputs: outputs}
			if status == "danger" && !branchRefs[step.Name] && !effectiveContinueOnError(step, jobDefs[StepKey(step)]) {
				return "danger"
			}

		case "sequence":
			// PS-1: an inline serial chain. Standalone (top level / branch arm) it
			// is just its steps — walked in place, sharing this walk's results.
			// Its real purpose is as a parallel ARM (below), where it gives one
			// concurrent lane a serial sub-chain.
			if e.walkSteps(ctx, wfTraceID, wfName, wfID, step.Steps, scope, actor, jobDefs, results) == "danger" {
				return "danger"
			}

		case "parallel":
			// PS-1: each element of Jobs is one concurrent ARM — a leaf job (the
			// original shape) or a sequence whose steps run serially inside the
			// arm. A sequence arm walks against its OWN CLONE of results (sibling
			// arms must not see each other mid-flight, mirroring validation's
			// scoping); its newly-produced results merge back as the arm finishes.
			type outcome struct {
				name       string
				status     string
				traceID    string
				continueOn bool
				produced   map[string]*JobResult // sequence arm: results it added
			}
			ch := make(chan outcome, len(step.Jobs))
			// WB-S4: bound the fan-out so a wide parallel block can't spawn an
			// unbounded number of concurrent child dispatches/pollers. The bound is
			// the global concurrency cap (settings.maxConcurrent), floored at 1.
			// ch is buffered to len(step.Jobs), so a goroutine never blocks writing
			// its outcome — it releases its slot as soon as runStep returns, which
			// keeps the dispatch loop making progress even before the collect loop
			// below starts draining. A sequence arm holds its slot for the whole
			// chain; a parallel nested inside it takes slots from its OWN semaphore,
			// so the two levels cannot deadlock each other.
			sem := make(chan struct{}, e.maxParallel(ctx))
			preBlock := make(map[string]bool, len(results))
			for name := range results {
				preBlock[name] = true
			}
			for _, j := range step.Jobs {
				if j.Type == "sequence" {
					armResults := make(map[string]*JobResult, len(results))
					maps.Copy(armResults, results)
					sem <- struct{}{}
					go func() {
						defer func() { <-sem }()
						st := e.walkSteps(ctx, wfTraceID, wfName, wfID, j.Steps, scope, actor, jobDefs, armResults)
						ch <- outcome{status: st, produced: armResults}
					}()
					continue
				}
				inputEnv := e.resolveInputs(j, results) // snapshot from prior steps
				sem <- struct{}{}
				go func() {
					defer func() { <-sem }()
					s, tid := e.runStep(ctx, wfTraceID, wfName, wfID, j, scope, actor, jobDefs, inputEnv)
					ch <- outcome{name: j.Name, status: s, traceID: tid, continueOn: effectiveContinueOnError(j, jobDefs[StepKey(j)])}
				}()
			}
			fatal := false
			for range step.Jobs {
				o := <-ch
				if o.produced != nil {
					// Sequence arm: merge what the arm produced (first writer wins
					// on a cross-arm name clash, which validation forbids anyway).
					for k, v := range o.produced {
						if !preBlock[k] {
							if _, exists := results[k]; !exists {
								results[k] = v
							}
						}
					}
					// The arm's internal halt/continue logic already ran inside its
					// walk; a "danger" verdict here means the chain genuinely failed.
					if o.status == "danger" {
						fatal = true
					}
					continue
				}
				results[o.name] = &JobResult{Status: o.status, TraceID: o.traceID, Outputs: e.loadOutputs(ctx, o.traceID)}
				if o.status == "danger" && !branchRefs[o.name] && !o.continueOn {
					fatal = true
				}
			}
			if fatal {
				return "danger"
			}

		case "branch":
			if step.Condition == nil || step.Pass == nil || step.Fail == nil {
				e.log.Warn("workflow: branch step missing condition/pass/fail", "step", step.Name)
				continue
			}
			passed := EvaluateCondition(step.Condition, results)
			var taken, skipped *Branch
			if passed {
				taken, skipped = step.Pass, step.Fail
			} else {
				taken, skipped = step.Fail, step.Pass
			}

			// Mark every job NODE in the skipped arm — recursing into nested
			// branches/parallel (PP-H8 c) so none is silently dropped.
			for _, node := range flattenJobNodes(skipped.Steps) {
				e.markSkipped(ctx, wfTraceID, wfName, *node, actor, jobDefs)
				results[node.Name] = &JobResult{Status: "skipped", TraceID: node.NodeID}
			}

			if e.walkSteps(ctx, wfTraceID, wfName, wfID, taken.Steps, scope, actor, jobDefs, results) == "danger" {
				return "danger"
			}
		}
	}
	return "success"
}

// runStep runs one job step with WB-R1 retry: the first attempt uses the step's
// pre-minted node id; each retry (up to the effective count) is a FRESH child run
// after the effective backoff, so every attempt is its own row in the timeline.
// Returns the final terminal status and the trace id of the final attempt (used
// for the results map and A12 output reads). Backoff and the per-attempt wait both
// honor ctx, so a cancel (WB-S2) stops retries promptly.
func (e *Engine) runStep(
	ctx context.Context,
	wfTraceID, wfName string,
	wfID int64,
	step Step,
	scope, actor string,
	jobDefs map[string]jobDef,
	inputEnv map[string]string,
) (string, string) {
	retries, backoff, _ := effectiveRetry(step, jobDefs[StepKey(step)])
	traceID := step.NodeID
	if traceID == "" {
		traceID = db.NewTraceID()
	}
	status := e.runJob(ctx, wfTraceID, wfName, wfID, step, traceID, scope, actor, jobDefs, inputEnv)
	for attempt := 1; attempt <= retries && status == "danger"; attempt++ {
		if ctx.Err() != nil {
			break
		}
		if backoff > 0 {
			t := time.NewTimer(time.Duration(backoff) * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return status, traceID
			case <-t.C:
			}
		}
		e.log.Info("workflow: retrying step", "job", step.Name, "attempt", attempt, "of", retries)
		traceID = db.NewTraceID() // each attempt is its own child run
		status = e.runJob(ctx, wfTraceID, wfName, wfID, step, traceID, scope, actor, jobDefs, inputEnv)
	}
	return status, traceID
}

// runJob enqueues a child run with the given trace id and polls until it reaches a
// terminal state.
func (e *Engine) runJob(
	ctx context.Context,
	wfTraceID, wfName string,
	wfID int64,
	step Step,
	traceID string,
	scope, actor string,
	jobDefs map[string]jobDef,
	inputEnv map[string]string,
) string {
	if traceID == "" {
		// Defense-in-depth: every job node is pre-minted by assignNodeIDs, but a
		// missing id must never become an empty-string PK (PP-H8 c).
		traceID = db.NewTraceID()
	}

	// Look up the job's run type.
	jd, ok := jobDefs[StepKey(step)]
	if !ok {
		e.log.Warn("workflow: job not found in DB", "job", step.Name)
		jd = jobDef{runType: "bash", scope: scope}
	}
	effectiveScope := scope
	if jd.scope != "" {
		effectiveScope = jd.scope
	}

	// Insert the child run directly (pre-assigned trace ID). Gap A: each child
	// inherits the parent workflow run's env snapshot so sshexec injection applies
	// uniformly to scheduled-workflow steps.
	// R5.1 — resolve the executor (job spec.executor > global default > capability),
	// source-qualified by the step's resolved job source (A11 precedence; v20 Phase 4).
	jobSrc := jd.source
	if jobSrc == "" {
		jobSrc = "git"
	}
	executor := scheduler.ResolveExecutor(ctx, e.db, jobSrc, step.Name, jd.runType)
	concKey := cronutil.ConcurrencyKey(jd.concurrencyKey, jd.uid, jobSrc, step.Name)
	// M3/T3.6 — snapshot the effective scope's agency SET onto the child run (hard
	// isolation). This path builds its own INSERT rather than going through
	// scheduler.EnqueueParams, which is exactly why it is easy to miss: a child run
	// left with an empty snapshot would look like general-pool work to claimRun and
	// be refused by every runner in its actual agency. TG-2 was the same class of
	// miss for target_host — the column was simply absent from the list below, so
	// every workflow step fanned out across its job's whole scope.
	stepAgencies, _ := execspec.ScopeAgencies(ctx, e.db, effectiveScope)
	stepAgenciesJSON := execspec.MarshalAgencies(stepAgencies)

	// RA-24 — the WORKFLOW half, and the one with no escape hatch. §13.3: a workflow
	// has no scope column and there is NO per-run scope override for one, so an
	// operator meeting this at run time cannot fix it by binding a scope the way a
	// job trigger can — the remedy is to scope the constituent JOB.
	//
	// The child run is still INSERTED, terminal, carrying the reason: the
	// orchestrator accounts for its steps by their child runs, so a silently skipped
	// step would leave the workflow waiting on a run that never existed. A failed
	// step is both true and something the walk already knows how to propagate.
	// Gated on the run actually being unbound before anything is read: jobDef carries
	// no script_ref, and widening the bulk loader to fetch one for every step of
	// every workflow would be a real cost for a check that only ever fires on the
	// unbound path.
	stepStatus, stepQueuedReason := "queued", ""
	// FX-A1 — a job the catalog bars (disabled, or in the recycle bin) fails its
	// step here, before any gate below can be reached. Terminal-and-recorded for
	// the same reason RA-24 gives: the orchestrator accounts for its steps by
	// their child runs, so a silently skipped step leaves the workflow waiting on
	// a run that never existed.
	if jd.unavailable != "" {
		stepStatus, stepQueuedReason = "failure", jd.unavailable
		e.log.Warn("workflow: step references a job that must not run",
			"job", step.Name, "source", jobSrc, "reason", jd.unavailable)
	}
	// R2F-1 — jd.uid is the job resolveJobDef settled on (R2-3); read the
	// script and the bindings off THAT identity, or a same-named sibling's
	// credentials decide whether this step is allowed to run.
	var stepScriptRef sql.NullString
	_ = e.db.QueryRowContext(ctx,
		`SELECT script_ref FROM jobs WHERE CASE WHEN ? != '' THEN uid = ? ELSE name = ? AND source = ? END`,
		jd.uid, jd.uid, step.Name, jobSrc).Scan(&stepScriptRef)
	stepOwners := runref.RunOwners(jobSrc, step.Name, jd.uid, stepScriptRef.String)
	if stepStatus == "queued" && effectiveScope == "" && len(stepAgencies) == 0 {
		blocked, berr := runref.UnboundRunBlocked(ctx, e.db, stepOwners, effectiveScope, stepAgencies)
		if berr != nil {
			e.log.Error("workflow: check unbound references", "job", step.Name, "err", berr)
			return "danger"
		}
		if len(blocked) > 0 {
			stepStatus, stepQueuedReason = "failure", runref.QueuedReasonUnboundReferences
			e.log.Warn("workflow: step consumes department-owned credentials on an unbound run",
				"job", step.Name, "reference", blocked[0].Reference, "detail", runref.UnboundRefusal(blocked))
		}
	}
	// KB — a key-bound step whose run resolves to the ssh executor fails the
	// step here, terminal-and-recorded like the two refusals above: the executor
	// cannot deliver the key, and a step that vanished would leave the workflow
	// waiting on a run that never existed.
	if stepStatus == "queued" {
		keys, kerr := runref.KeyBindingsOnSSH(ctx, e.db, stepOwners, executor)
		if kerr != nil {
			e.log.Error("workflow: check key bindings", "job", step.Name, "err", kerr)
			return "danger"
		}
		if len(keys) > 0 {
			stepStatus, stepQueuedReason = "failure", runref.ReasonKeyBindingOnSSH
			e.log.Warn("workflow: step binds an SSH key but resolved to the ssh executor",
				"job", step.Name, "reference", keys[0].Reference, "detail", runref.KeyBindingRefusal(keys))
		}
	}

	// RR-2: one writer. This engine used to build its own child-run INSERT —
	// "the recurring reason things get missed here" — and it missed
	// requires_json, checkout_* and runner_tag (RR-0b/c). It now describes the
	// row and scheduler.InsertRun writes it with the same column list every
	// other producer uses. The pieces that were computed in SQL before are
	// computed here from the same sources:
	//
	// RX — a child step INHERITS its parent workflow run's reaction depth,
	// unchanged. A workflow's internal steps are not reaction hops: the hop was
	// the reaction that started the workflow, and the steps are that one unit
	// of work executing. Without this the depth ceiling has a hole exactly
	// where §2.8 says it is needed — a cycle routed reaction → workflow → step →
	// reaction would reset the counter every lap. Read from the parent ROW, not
	// threaded down the walk: the row is the truth, a carried copy can drift.
	var parentDepth int
	_ = e.db.QueryRowContext(ctx,
		`SELECT COALESCE(reaction_depth, 0) FROM workflow_runs WHERE id = ?`, wfTraceID,
	).Scan(&parentDepth)
	if _, err := scheduler.InsertRun(ctx, e.db, scheduler.RunRow{
		EnqueueParams: scheduler.EnqueueParams{
			JobName:        step.Name,
			JobSource:      jobSrc,
			JobUID:         jd.uid,
			RunType:        jd.runType,
			Scope:          effectiveScope,
			TargetHost:     jd.targetHost,
			TriggerKind:    "workflow",
			TriggeredBy:    actor,
			ConcurrencyKey: concKey,
			WorkflowRunID:  wfTraceID,
			Executor:       executor,
			EnvJSON:        e.childEnvJSON(ctx, wfTraceID, jobSrc, step.Name, inputEnv).String,
			SSHUser:        jd.sshUser,
			SSHCredential:  jd.sshCred,
			AgenciesJSON:   stepAgenciesJSON,
			ReactionDepth:  parentDepth,
		},
		TraceID:      traceID,
		Status:       stepStatus,
		QueuedReason: stepQueuedReason,
	}); err != nil {
		e.log.Error("workflow: insert child run", "job", step.Name, "err", err)
		return "danger"
	}
	if err := execspec.SyncRunAgencies(ctx, e.db, traceID, stepAgenciesJSON); err != nil {
		// Fail the step rather than enqueue a run the claim predicate would mis-route.
		e.log.Error("workflow: materialize child run agencies", "job", step.Name, "err", err)
		return "danger"
	}
	// RA-20(b) — same advisory hint the scheduler/trigger paths get from
	// EnqueueRunWithID. This engine builds its own INSERT (the recurring reason
	// things get missed here — see the agencies comment above), so it calls the
	// helper directly. Skipped for a step already terminal from RA-24: it has a
	// reason, and it is not queued.
	if stepStatus == "queued" {
		if reason, rerr := execspec.StampUnclaimableReason(ctx, e.db, traceID); rerr != nil {
			e.log.Warn("workflow: stamp unclaimable reason", "job", step.Name, "err", rerr)
		} else if reason != "" {
			e.log.Info("workflow: step queued with no eligible runner",
				"job", step.Name, "reason", reason)
		}
	}

	// Poll until terminal, bounded by the job's timeout (+ grace) so a hung or
	// non-emitting step can't pin the walk goroutine forever (A12/§13.2).
	var maxWait time.Duration
	if jd.timeout > 0 {
		maxWait = time.Duration(jd.timeout)*time.Second + 60*time.Second
	}
	return e.waitForRun(ctx, traceID, maxWait)
}

// effectiveRetry resolves a step's retry config (WB-R1, D3): the per-step value
// when set, otherwise the job-definition default. Negative values are clamped to 0.
func effectiveRetry(step Step, jd jobDef) (retries, backoff int, continueOnError bool) {
	// FX-A1 — a catalog refusal is not a transient failure, so it is not retried.
	// Every attempt would re-read the same barred row and insert another identical
	// terminal child run, sleeping the backoff between. continue_on_error is left
	// to the normal resolution below: whether the WALK survives a failed step is a
	// separate decision from whether the step is worth attempting again.
	if jd.unavailable != "" {
		return 0, 0, effectiveContinueOnError(step, jd)
	}
	retries = jd.retries
	if step.Retries != nil {
		retries = *step.Retries
	}
	backoff = jd.backoffSeconds
	if step.BackoffSeconds != nil {
		backoff = *step.BackoffSeconds
	}
	continueOnError = jd.continueOnError
	if step.ContinueOnError != nil {
		continueOnError = *step.ContinueOnError
	}
	if retries < 0 {
		retries = 0
	}
	if backoff < 0 {
		backoff = 0
	}
	return
}

// effectiveContinueOnError resolves whether a still-failing step should be
// skipped-and-proceeded rather than halt the workflow (step value ?? job default).
func effectiveContinueOnError(step Step, jd jobDef) bool {
	if step.ContinueOnError != nil {
		return *step.ContinueOnError
	}
	return jd.continueOnError
}

// ─── Cancellation (WB-S2) ──────────────────────────────────────────────────────

func (e *Engine) registerCancel(traceID string, cancel context.CancelFunc) {
	e.mu.Lock()
	e.cancels[traceID] = cancel
	e.mu.Unlock()
}

func (e *Engine) unregisterCancel(traceID string) {
	e.mu.Lock()
	delete(e.cancels, traceID)
	e.mu.Unlock()
}

// runCancelled reports whether a soft-cancel was persisted for this run. Checked
// between steps so cancellation also halts a walk driven by an engine instance
// that doesn't hold the cancel func (e.g. the scheduler's per-trigger engine).
func (e *Engine) runCancelled(ctx context.Context, wfTraceID string) bool {
	var c int
	_ = e.db.QueryRowContext(ctx, `SELECT cancelled FROM workflow_runs WHERE id = ?`, wfTraceID).Scan(&c)
	return c == 1
}

// Cancel soft-cancels a running workflow (WB-S2 / D4): it flags the workflow run
// cancelled, marks not-started (still-queued) child runs skipped so they aren't
// picked up, and cancels the walk context so in-flight waits return and no further
// steps dispatch. In-flight child runs are left to finish (soft — no runner kill).
// Idempotent. Returns true when a running run was actually transitioned (so the
// endpoint can 404/409 a finished or unknown run).
func (e *Engine) Cancel(ctx context.Context, wfTraceID string) bool {
	now := time.Now().UTC().Format(time.RFC3339)
	// Only a still-running, not-yet-cancelled run transitions (status stays a
	// CHECK-legal terminal; the serializers map cancelled=1 to display 'cancelled').
	res, err := e.db.ExecContext(ctx,
		`UPDATE workflow_runs SET cancelled = 1, cancelled_at = ? WHERE id = ? AND status = 'running' AND cancelled = 0`, now, wfTraceID)
	if err != nil {
		e.log.Error("workflow: flag cancel", "trace", wfTraceID, "err", err)
		return false
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false // not running / unknown / already cancelled
	}
	// Not-started children (dispatched but not yet picked up) become skipped.
	//
	// FX-D1 — with a REASON. These rows are indistinguishable in the runs table
	// from a calendar veto or a Forbid refusal, and now that a job's most recent
	// suppression is surfaced on its own (lastSkippedAt/lastSkipReason), a row
	// with no reason renders as a blank explanation next to a timestamp. The
	// operator who cancelled the workflow knows why; the job page does not,
	// unless the row says so.
	_, _ = e.db.ExecContext(ctx,
		`UPDATE runs SET status = 'skipped', completed_at = ?,
		        queued_reason = COALESCE(queued_reason, 'Skipped: the workflow was cancelled before this step started')
		  WHERE workflow_run_id = ? AND status = 'queued'`, now, wfTraceID)

	e.mu.Lock()
	cancel := e.cancels[wfTraceID]
	e.mu.Unlock()
	if cancel != nil {
		cancel() // stop in-flight waits + further dispatch
	}
	return true
}

// childEnvJSON builds a child run's env snapshot in precedence order (later wins):
// job-level env (JC10/JC11 base) → the parent workflow env → this step's resolved
// A12 inputs (inputs win). Returns NULL when every layer is empty (R2 no-op).
func (e *Engine) childEnvJSON(ctx context.Context, wfTraceID, jobSource, jobName string, inputEnv map[string]string) sql.NullString {
	var jobEnv sql.NullString
	_ = e.db.QueryRowContext(ctx, `SELECT env_json FROM jobs WHERE name=? AND source=?`, jobName, jobSource).Scan(&jobEnv)
	parent := e.parentEnvJSON(ctx, wfTraceID)
	merged := envmerge.Merge(envmerge.Parse(jobEnv.String), envmerge.Parse(parent.String), inputEnv)
	if merged == nil {
		return sql.NullString{}
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// resolveInputs maps a step's declared inputs to upstream captured outputs (A12 /
// Q-F). A missing/failed upstream output resolves to "" (deterministic).
func (e *Engine) resolveInputs(step Step, results map[string]*JobResult) map[string]string {
	if len(step.Inputs) == 0 {
		return nil
	}
	env := make(map[string]string, len(step.Inputs))
	for key, ref := range step.Inputs {
		if up, ok := results[ref.FromStep]; ok && up.Outputs != nil {
			env[key] = up.Outputs[ref.FromOutput]
		} else {
			env[key] = ""
		}
	}
	return env
}

// loadOutputs reads a finished child run's captured A12 outputs (nil if none).
func (e *Engine) loadOutputs(ctx context.Context, traceID string) map[string]string {
	var js sql.NullString
	_ = e.db.QueryRowContext(ctx, `SELECT outputs_json FROM runs WHERE id = ?`, traceID).Scan(&js)
	if !js.Valid || js.String == "" {
		return nil
	}
	m := map[string]string{}
	if json.Unmarshal([]byte(js.String), &m) != nil {
		return nil
	}
	return m
}

// parentEnvJSON returns the env snapshot persisted on the parent workflow run
// (NULL when the workflow carried no schedule env), copied onto each child run.
func (e *Engine) parentEnvJSON(ctx context.Context, wfTraceID string) sql.NullString {
	var env sql.NullString
	_ = e.db.QueryRowContext(ctx, `SELECT env_json FROM workflow_runs WHERE id = ?`, wfTraceID).Scan(&env)
	return env
}

// waitForRun polls the runs table until the run reaches a terminal status, or
// until maxWait elapses (0 ⇒ no engine-side bound; the executor enforces the job
// timeout). The bound is a backstop against a hung/non-emitting step (A12/§13.2).
func (e *Engine) waitForRun(ctx context.Context, traceID string, maxWait time.Duration) string {
	var deadline <-chan time.Time
	if maxWait > 0 {
		t := time.NewTimer(maxWait)
		defer t.Stop()
		deadline = t.C
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "danger"
		case <-deadline:
			e.log.Warn("workflow: step exceeded engine wait bound", "trace", traceID)
			return "danger"
		case <-ticker.C:
			var status string
			err := e.db.QueryRowContext(ctx,
				`SELECT status FROM runs WHERE id = ?`, traceID,
			).Scan(&status)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "danger"
				}
				e.log.Error("workflow: poll run status", "trace", traceID, "err", err)
				continue
			}
			switch status {
			case "success", "warning":
				return "success"
			case "failure", "killed", "danger":
				return "danger"
			case "skipped":
				return "skipped"
			}
			// queued or running — keep waiting.
		}
	}
}

// markSkipped records a skipped child run row, keyed by the node's pre-minted
// trace ID (PP-H8 c).
func (e *Engine) markSkipped(
	ctx context.Context,
	wfTraceID, wfName string,
	step Step,
	actor string,
	jobDefs map[string]jobDef,
) {
	jobName := step.Name
	traceID := step.NodeID
	if traceID == "" {
		traceID = db.NewTraceID()
	}
	// R2F-2: keyed like every other defs read, so a skipped step records the
	// SAME job the walk would have run — including which twin, when it pins one.
	jd := jobDefs[StepKey(step)]
	// runType fallback (PP-H8 d): an unresolved job ref has run_type='' which
	// violates the runs.run_type CHECK, so the skipped row would silently fail to
	// insert. Default to bash, mirroring runJob.
	if jd.runType == "" {
		jd.runType = "bash"
	}
	jobSrc := jd.source
	if jobSrc == "" {
		jobSrc = "git"
	}
	concKey := cronutil.ConcurrencyKey(jd.concurrencyKey, jd.uid, jobSrc, jobName)
	// TG-2: target_host is carried here purely so a skipped row DISPLAYS the pin the
	// step would have run against — a skipped run never executes, so this has no
	// blast-radius meaning.
	// RR-2: one writer. A skipped step carries what it was about and where it
	// would have pointed, not how it would have run — scope stays NULL as it
	// always has on this path, and Terminal takes no dispatch snapshot.
	if _, err := scheduler.InsertRun(ctx, e.db, scheduler.RunRow{
		EnqueueParams: scheduler.EnqueueParams{
			JobName:        jobName,
			JobSource:      jobSrc,
			JobUID:         jd.uid,
			RunType:        jd.runType,
			TargetHost:     jd.targetHost,
			TriggerKind:    "workflow",
			TriggeredBy:    actor,
			ConcurrencyKey: concKey,
			WorkflowRunID:  wfTraceID,
		},
		TraceID:  traceID,
		Status:   "skipped",
		Terminal: true,
		// FX-D1 — a reason, for the same argument as the cancel path: this row
		// is surfaced as the job's most recent suppression, and one with no
		// reason renders as a blank next to a timestamp.
		QueuedReason: "Skipped: an earlier step in the workflow failed",
	}); err != nil {
		e.log.Error("workflow: insert skipped run", "job", jobName, "err", err)
	}
}

// finishWorkflow updates workflow_runs to terminal and emits workflow-end.
func (e *Engine) finishWorkflow(
	ctx context.Context,
	wfTraceID, wfName string,
	wfID int64,
	finalStatus string, // "success" | "danger"
	actor string,
) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Compute duration_ms.
	var startedAt sql.NullString
	_ = e.db.QueryRowContext(ctx, `SELECT started_at FROM workflow_runs WHERE id = ?`, wfTraceID).Scan(&startedAt)
	var durationMs int64
	if startedAt.Valid {
		if t, err := time.Parse(time.RFC3339, startedAt.String); err == nil {
			durationMs = time.Since(t).Milliseconds()
		}
	}

	dbStatus := finalStatus
	if dbStatus == "danger" {
		dbStatus = "failure"
	}

	// Idempotency guard (PP-H8 b): only the transition out of 'running' finalizes.
	// runWorkflow already calls this exactly once, but the status-gated UPDATE +
	// RowsAffected check make a double-call (e.g. a future caller) a no-op rather
	// than a duplicate workflow-end / duration.
	res, err := e.db.ExecContext(ctx, `
		UPDATE workflow_runs SET status = ?, completed_at = ?, duration_ms = ?
		WHERE id = ? AND status = 'running'
	`, dbStatus, now, durationMs, wfTraceID)
	if err != nil {
		e.log.Error("workflow: update workflow_run terminal", "trace", wfTraceID, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // already finalized — do not double-emit workflow-end
	}

	outcome := "success"
	if finalStatus == "danger" {
		outcome = "failure"
	}
	if err := EmitActivity(ctx, e.db, ActivityParams{
		Kind:         "workflow-end",
		Outcome:      outcome,
		Actor:        actor,
		WorkflowName: wfName,
		WorkflowID:   wfID,
		TraceID:      wfTraceID,
		DurationMs:   durationMs,
	}); err != nil {
		e.log.Error("workflow: emit workflow-end", "err", err)
	}
}

// ─── Job definition lookup ────────────────────────────────────────────────────

type jobDef struct {
	runType string
	scope   string
	source  string // git | amadeus — resolved via the A11 step-source precedence
	// targetHost is the definition's single-host pin (TG-2). Empty ⇒ scope fan-out.
	// Carried here because this engine builds its own child-run INSERT rather than
	// going through scheduler.EnqueueParams, so a field the params struct delivers
	// for free on other paths has to be threaded explicitly on this one.
	targetHost string
	// sshUser/sshCred (CA-10) — the job-spec "connect as" identity, frozen onto
	// the child run like targetHost. Threaded for the same reason: this engine
	// builds its own INSERT. Folded only for ssh-family run types (the
	// ansible/terraform identity comes from inventory/toolchain).
	sshUser string
	sshCred string
	timeout int // timeout_seconds (0 ⇒ none); bounds the engine's wait (A12/§13.2)
	// uid is the resolved job's permanent identity (R2-3). It is what the child
	// run's concurrency key is built from, so a workflow step gates against the
	// same job a cron fire does — the RX-25 agreement, now keyed on identity.
	uid             string
	concurrencyKey  string
	retries         int  // WB-R1 job-level default (step value overrides)
	backoffSeconds  int  // WB-R1 job-level default
	continueOnError bool // WB-R1 job-level default
	// unavailable (FX-A1) is the refusal reason when the step's job EXISTS but the
	// catalog says it must not run — disabled, or sitting in the recycle bin. It
	// cannot be expressed by simply failing to resolve: an unresolved name falls
	// back to a synthetic bash def (see runJob), so a binned job would still have
	// executed, as bash, with the workflow's scope. A def carrying this instead
	// makes the step terminal-failure with the reason on the child run, which is
	// the same shape RA-24 and the sub-workflow step's refusal already use.
	unavailable string
}

// StepRef is one distinct job reference in a step graph: the name the step
// displays, the A11 source override it declares, and — since R2F-2 — the
// identity it pins, when it pins one.
type StepRef struct {
	Name   string
	Source string
	UID    string
}

// StepRefOf reads a step's job reference.
func StepRefOf(s Step) StepRef {
	return StepRef{Name: s.Name, Source: s.Source, UID: s.JobUID}
}

// Key is what a resolved jobDef is filed under, and what makes two references
// "the same job" for de-duplication: the identity when the step pins one, the
// name otherwise. The prefixes keep the two namespaces apart, so a job whose
// NAME happens to look like a uid cannot collide with a real identity.
//
// This is why the defs map is keyed by Key and not by Name: two steps pinning
// DIFFERENT twins of one name are two references, and a name-keyed map would
// collapse them — resolving both to whichever the walk reached last, which is
// the silent substitution R2F-2 exists to make impossible.
func (r StepRef) Key() string {
	if r.UID != "" {
		return "uid:" + r.UID
	}
	return "name:" + r.Name
}

// StepKey is the defs-map key for a step, the lookup half of StepRef.Key.
func StepKey(s Step) string { return StepRefOf(s).Key() }

// lookupJobDefs resolves each step's referenced job def. A step pinning a uid
// (R2F-2) resolves by identity alone; otherwise the A11 step-source precedence
// applies (v20 Phase 4): an explicit step.Source wins; otherwise the parent
// workflow's source; otherwise the other source if the job exists there. A ref
// absent from the returned map is unresolved (runJob warns).
func (e *Engine) lookupJobDefs(ctx context.Context, steps []Step, wfSource string) (map[string]jobDef, error) {
	if wfSource == "" {
		wfSource = "git"
	}
	refs := collectStepRefs(steps)
	m := make(map[string]jobDef, len(refs))
	for _, ref := range refs {
		src, jd, ok := e.resolveJobDef(ctx, ref, wfSource)
		if !ok {
			continue
		}
		jd.source = src
		m[ref.Key()] = jd
	}
	return m, nil
}

// maxParallel bounds the number of concurrent child dispatches inside a parallel
// step (WB-S4). It reads the global concurrency cap (settings.maxConcurrent,
// default 10) and floors it at 1 so a parallel block always makes progress.
func (e *Engine) maxParallel(ctx context.Context) int {
	var val sql.NullString
	_ = e.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'maxConcurrent'`).Scan(&val)
	if n, err := strconv.Atoi(val.String); err == nil && n >= 1 {
		return n
	}
	return 10
}

// JobScopes returns the distinct effective scopes of every job referenced
// anywhere in the step graph, applying the same A11 step-source precedence the
// engine uses at run time. Its only caller is the API's workflow authorization
// guard (workflowScopesPermit), which enforces scope access and the RB-2 verb on
// the workflow trigger/patch/cancel the way runJob does per job.
//
// ⚠️ An UNSCOPED constituent job yields an EMPTY STRING entry, and that entry is
// load-bearing. This function used to filter empties out, which made the caller's
// `sc == ""` branch — the RB-26 rule that an unscoped job inside a workflow is
// unrestricted-only — unreachable dead code: a workflow of entirely unscoped jobs
// produced an empty list and therefore no check at all. Fixed in RF-6
// (the RBAC-fixes plan). If this is ever changed back to skipping
// empties, that guard silently stops running.
func (e *Engine) JobScopes(ctx context.Context, steps []Step, wfSource string) ([]string, error) {
	defs, err := e.lookupJobDefs(ctx, steps, wfSource)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, jd := range defs {
		if !seen[jd.scope] {
			seen[jd.scope] = true
			out = append(out, jd.scope)
		}
	}
	return out, nil
}

// resolveJobDef finds a step's job def: by identity when the step pins one
// (R2F-2), otherwise at the effective source per the A11 precedence.
func (e *Engine) resolveJobDef(ctx context.Context, ref StepRef, wfSource string) (string, jobDef, bool) {
	name := ref.Name
	// scan runs the def query with an arbitrary WHERE and returns the row's own
	// source, so the identity arm reports where the job actually lives rather
	// than where the step guessed it would.
	scan := func(where string, args ...any) (string, jobDef, bool) {
		var rt, sc, th, su, scr, rowSrc string
		var to, retries, backoff int
		var continueOnErr bool
		var ck sql.NullString
		var disabled, binned int
		var uid string
		err := e.db.QueryRowContext(ctx,
			`SELECT run_type, COALESCE(scope,''), COALESCE(target_host,''), COALESCE(timeout_seconds,0), concurrency_key,
			        COALESCE(retries,0), COALESCE(backoff_seconds,0), COALESCE(continue_on_error,0),
			        COALESCE(ssh_user,''), COALESCE(ssh_credential,''),
			        enabled = 0, deleted_at IS NOT NULL, COALESCE(uid,''), source
			 FROM jobs WHERE `+where,
			args...).Scan(&rt, &sc, &th, &to, &ck, &retries, &backoff, &continueOnErr, &su, &scr, &disabled, &binned, &uid, &rowSrc)
		if err != nil {
			return "", jobDef{}, false
		}
		// CA-10 — the identity fold is gated to identity-capable run types here
		// (RP-7), at the single read point, so neither INSERT below can freeze it
		// onto a run the dispatch path would 409/ignore.
		if !execspec.IdentityCapableRunType(rt) {
			su, scr = "", ""
		}
		jd := jobDef{runType: rt, scope: sc, targetHost: th, sshUser: su, sshCred: scr, timeout: to, uid: uid, concurrencyKey: ck.String, retries: retries, backoffSeconds: backoff, continueOnError: continueOnErr}
		// FX-A1 — the row EXISTS, so A11 precedence has already chosen this source;
		// whether it may RUN is a separate question, answered here.
		//
		// This distinction is the whole fix. Filtering `enabled = 1 AND deleted_at
		// IS NULL` in the WHERE instead turns the precedence loop from "the first
		// source where the name exists" into "the first source where it is
		// runnable" — so binning a job would not refuse it, it would silently
		// substitute the SAME-NAMED job at the other source: a different script, run
		// type, scope and agency snapshot, reported as success. A11 (see
		// lookupJobDefs) resolves on existence, and the barred row still holds its
		// name here, so the search must stop at it.
		//
		// Every other field is carried through even for a barred job: the terminal
		// child run stays honest, JobScopes still authorizes against the real scope,
		// and continue_on_error still decides whether the WALK halts — a job the
		// operator marked best-effort must not become workflow-fatal by being binned.
		switch {
		case binned == 1:
			jd.unavailable = "Refused: the referenced job is in the recycle bin"
		case disabled == 1:
			jd.unavailable = "Refused: the referenced job is disabled"
		}
		return rowSrc, jd, true
	}

	// R2F-2 — an explicit identity is the whole resolution: one indexed lookup,
	// no source-precedence walk. A pinned step says WHICH job, so there is no
	// fallback to make, and a uid that no longer resolves must REFUSE rather than
	// drop back to a same-named job — the RX purge-recreate semantics, where a
	// new identity is a new job on purpose. This is also what makes a twin pair
	// buildable: the ambiguity refusal below is unreachable from a pinned step.
	if ref.UID != "" {
		if src, jd, ok := scan(`uid = ?`, ref.UID); ok {
			return src, jd, true
		}
		// Dangling. Report at the step's own effective source so the terminal
		// child run still carries a legal source value.
		return StepSourceOrder(ref.Source, wfSource)[0], jobDef{runType: "bash",
			unavailable: "Refused: the job this step points to no longer exists"}, true
	}

	for _, src := range StepSourceOrder(ref.Source, wfSource) {
		// R2-5 — a name may match two jobs within one source pool now. COUNT
		// first: an ambiguous reference REFUSES (fail closed, RA-17) rather than
		// running whichever row SQLite returns — a step that silently picks a
		// department's job is the substitution hazard the FX-A1 comment above
		// describes, made worse. R2F-2 gives the operator the way out: re-pick
		// the job in the editor and the step carries its identity instead.
		var nMatches int
		_ = e.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE name = ? AND source = ?`, name, src).Scan(&nMatches)
		if nMatches > 1 {
			return src, jobDef{runType: "bash", source: src,
				unavailable: "Refused: this name matches more than one job; qualify the step by identity"}, true
		}
		if _, jd, ok := scan(`name = ? AND source = ?`, name, src); ok {
			return src, jd, true
		}
	}
	return "", jobDef{}, false
}

// collectStepRefs walks the step graph and returns the distinct job references
// it makes, in first-appearance order.
//
// R2F-2 turned this from a name → source map into a list of StepRefs, because a
// name is no longer enough to say which job a step means. Two refs that share a
// Key collapse with LAST write winning the value — the map's old semantics, kept
// so a graph whose two same-named steps disagree about jobSource resolves
// exactly as it did before, while the ORDER (first appearance) is what the
// compose-time checks iterate, so their messages and audit rows stay stable.
//
// PS-1b: this walk must reach EVERY job node, by exactly the same recursion
// flattenJobNodes uses. It previously stopped at a parallel arm's own name and
// had no sequence case at all, so a job inside a sequence arm never entered the
// list — and a ref absent from here is absent from lookupJobDefs, which makes
// runJob fall back to a synthetic jobDef (bash, the workflow's scope) and keeps
// the job out of Engine.JobScopes, so workflowScopesPermit authorizes the run
// without ever consulting that job's scope.
func collectStepRefs(steps []Step) []StepRef {
	var out []StepRef
	at := map[string]int{} // Key → index in out
	add := func(ref StepRef) {
		if i, ok := at[ref.Key()]; ok {
			out[i] = ref // last write wins the value, first appearance keeps the slot
			return
		}
		at[ref.Key()] = len(out)
		out = append(out, ref)
	}
	var walk func([]Step)
	walk = func(ss []Step) {
		for _, s := range ss {
			switch stepKind(s) {
			case "job":
				if s.Name != "" {
					add(StepRefOf(s))
				}
			case "parallel":
				// An arm is a leaf job or a sequence — recursing handles both.
				walk(s.Jobs)
			case "sequence":
				walk(s.Steps)
			case "branch":
				if s.Pass != nil {
					walk(s.Pass.Steps)
				}
				if s.Fail != nil {
					walk(s.Fail.Steps)
				}
			case stepKindWorkflow:
				// SW: a workflow step references no JOB of its own. Its children
				// resolve their own defs when the child run triggers, against the
				// CHILD's source — deliberately not flattened into the parent's
				// lookup, which would make a rename in one workflow able to break
				// another's job resolution.
			}
		}
	}
	walk(steps)
	return out
}

// ─── Shared helpers ───────────────────────────────────────────────────────────

// stepKind returns a step's effective type, resolving the empty type to "job".
//
// ValidateSteps legislates that an omitted type means "job" (a bare
// {name: deploy} is a legal step), but the walkers each switched on Step.Type
// raw, so an untyped step matched no case: it received no node ID, never
// entered the job-defs lookup, and was silently skipped at execution. Every
// switch over a step's type goes through here so the engine agrees with the
// validator about what a graph contains.
// stepKindWorkflow is the SW step type: a step that runs another workflow.
const stepKindWorkflow = "workflow"

func stepKind(s Step) string {
	if s.Type == "" {
		return "job"
	}
	return s.Type
}

// FlattenSteps returns an ordered list of all job names across all arms of the
// step graph, recursing into nested branches (PP-H8 c — the prior single-level
// version missed jobs in nested branches).
func FlattenSteps(steps []Step) []string {
	nodes := flattenJobNodes(steps)
	jobs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		jobs = append(jobs, n.Name)
	}
	return jobs
}

// flattenJobNodes returns pointers to every job NODE in the graph in order,
// recursing into nested branches and parallel blocks. Pointers index into the
// passed slice so callers can read/assign per-node fields (e.g. nodeID).
func flattenJobNodes(steps []Step) []*Step {
	var out []*Step
	var walk func([]Step)
	walk = func(ss []Step) {
		for i := range ss {
			switch stepKind(ss[i]) {
			case "job":
				out = append(out, &ss[i])
			case "parallel":
				// PS-1: an arm is a leaf job or a sequence — walking the arm list
				// handles both (and keeps the pointer-into-slice contract).
				walk(ss[i].Jobs)
			case "sequence":
				walk(ss[i].Steps)
			case "branch":
				if ss[i].Pass != nil {
					walk(ss[i].Pass.Steps)
				}
				if ss[i].Fail != nil {
					walk(ss[i].Fail.Steps)
				}
			}
		}
	}
	walk(steps)
	return out
}

// flattenWorkflowNodes returns pointers to every SUB-WORKFLOW node (SW).
//
// Separate from flattenJobNodes rather than folded into it: a workflow step
// produces a workflow_runs row, not a runs row, so it must NOT appear in
// jobTraceIDs (which callers read as "the child job runs") — but it still needs
// a minted node id, both to key its child's parent_node_id and so the canvas can
// address it.
func flattenWorkflowNodes(steps []Step) []*Step {
	var out []*Step
	var walk func([]Step)
	walk = func(ss []Step) {
		for i := range ss {
			switch stepKind(ss[i]) {
			case stepKindWorkflow:
				out = append(out, &ss[i])
			case "parallel":
				walk(ss[i].Jobs)
			case "sequence":
				walk(ss[i].Steps)
			case "branch":
				if ss[i].Pass != nil {
					walk(ss[i].Pass.Steps)
				}
				if ss[i].Fail != nil {
					walk(ss[i].Fail.Steps)
				}
			}
		}
	}
	walk(steps)
	return out
}

// assignNodeIDs mints a fresh trace ID for every job node in the graph (PP-H8).
// Mutates the steps in place (slices/branch pointers are shared with the walk).
func assignNodeIDs(steps []Step) {
	for _, n := range flattenJobNodes(steps) {
		n.NodeID = db.NewTraceID()
	}
	// SW: sub-workflow nodes get ids too. Without one, a parent with two
	// workflow steps cannot tell its children apart and the canvas cannot place
	// either of them.
	for _, n := range flattenWorkflowNodes(steps) {
		n.NodeID = db.NewTraceID()
	}
}

// EvaluateCondition evaluates a branch condition against accumulated job
// results (matches prototype's evaluateCondition).
func EvaluateCondition(cond *Condition, results map[string]*JobResult) bool {
	if cond == nil {
		return false
	}
	ref, ok := results[cond.JobRef]
	if !ok || ref == nil {
		return false
	}
	switch cond.Type {
	case "job_status":
		return ref.Status == "success"
	case "output_match":
		actual, ok := ref.Outputs[cond.Field]
		if !ok {
			return false
		}
		switch cond.Operator {
		case "==":
			return actual == cond.Value
		case "!=":
			return actual != cond.Value
		case "contains":
			return strings.Contains(actual, cond.Value)
		}
	}
	return false
}

// collectBranchRefs returns the set of job names referenced by branch
// conditions (failures in those jobs shouldn't halt the workflow).
func collectBranchRefs(steps []Step) map[string]bool {
	refs := map[string]bool{}
	var collect func([]Step)
	collect = func(ss []Step) {
		for _, s := range ss {
			switch s.Type {
			case "branch":
				if s.Condition != nil {
					refs[s.Condition.JobRef] = true
				}
				if s.Pass != nil {
					collect(s.Pass.Steps)
				}
				if s.Fail != nil {
					collect(s.Fail.Steps)
				}
			case "parallel": // PS-1: arms may hold sequences holding branches
				collect(s.Jobs)
			case "sequence":
				collect(s.Steps)
			}
		}
	}
	collect(steps)
	return refs
}

// snapshotSteps serializes a step graph (with its minted node IDs) to JSON plus a
// sha256 hash, for the per-run reproducibility snapshot (WB-D1). Returns ("","")
// for an empty graph so the columns stay NULL.
func snapshotSteps(steps []Step) (string, string) {
	if len(steps) == 0 {
		return "", ""
	}
	b, err := json.Marshal(steps)
	if err != nil {
		return "", ""
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:])
}

// ParseSteps unmarshals the JSON steps column from the workflows table.
func ParseSteps(raw string) ([]Step, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var steps []Step
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil, fmt.Errorf("parse workflow steps: %w", err)
	}
	return steps, nil
}

// ─── Structural validation (WB-S5) ─────────────────────────────────────────────

// ValidationError is one structural/enum problem found in a step graph. Step
// names the offending step (or its parent for shape errors); Field narrows it to
// a property where useful. Serialized into the dry-run (WB-A1) response and the
// write-path 422 details.
type ValidationError struct {
	Step    string `json:"step,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ValidateSteps checks a step graph's structure and enums (WB-S5) without
// touching the DB — referential checks (job-existence, same-path duplicate) live
// at the call site. It recurses into branch arms so a nested git-authored graph
// is validated to the same standard as an in-app one. An empty graph is valid.
//
// Checks:
//   - Step.Type ∈ {job, parallel, branch, sequence} (empty defaults to job).
//   - a job step has a Name and no jobs[]/steps[]/branch fields.
//   - a parallel step has ≥1 arm; each arm is a named leaf job OR a sequence
//     (PS-1 — a serial chain running concurrently with its sibling arms).
//   - a sequence step has ≥1 step; its steps validate as an ordered sub-walk.
//   - a branch step has Condition + Pass + Fail; Condition.Type ∈
//     {job_status, output_match}; for output_match, Operator ∈ {==,!=,contains}
//     and Field is set; Condition.JobRef is set.
//   - every Inputs[].FromStep references a step name produced upstream.
//     Concurrent parallel arms cannot see each other's producers; a sequence
//     arm's own steps see earlier steps of the same arm.
func ValidateSteps(steps []Step) []ValidationError {
	errs := []ValidationError{}
	upstream := map[string]bool{}
	validateStepSeq(steps, upstream, &errs)
	return errs
}

// validateStepSeq validates one ordered sequence of steps. `upstream` holds the
// names of steps produced before this sequence; it is extended in place as each
// step's outputs become available to its successors (so A12 input refs resolve to
// real upstream producers, and siblings in a parallel block can't see each other).
func validateStepSeq(steps []Step, upstream map[string]bool, errs *[]ValidationError) {
	for i := range steps {
		step := steps[i]
		typ := step.Type
		if typ == "" {
			typ = "job"
		}
		switch typ {
		case "job":
			if step.Name == "" {
				addStepErr(errs, step, "name", "job step requires a name")
			}
			if len(step.Jobs) > 0 {
				addStepErr(errs, step, "jobs", "jobs[] is only valid on a parallel step")
			}
			if len(step.Steps) > 0 {
				addStepErr(errs, step, "steps", "steps[] is only valid on a sequence step")
			}
			if step.Pass != nil || step.Fail != nil || step.Condition != nil {
				addStepErr(errs, step, "type", "condition/pass/fail are only valid on a branch step")
			}
			validateInputs(step, upstream, errs)
			validateRetry(step, errs)
			if step.Name != "" {
				upstream[step.Name] = true
			}

		case "sequence":
			if len(step.Steps) == 0 {
				addStepErr(errs, step, "steps", "sequence step requires at least one step")
			}
			// A standalone sequence is an inline serial chain: its steps extend
			// upstream in place, exactly as if they were written at this level.
			validateStepSeq(step.Steps, upstream, errs)

		case "parallel":
			if len(step.Jobs) == 0 {
				addStepErr(errs, step, "jobs", "parallel step requires at least one arm")
			}
			// Validate each arm against the upstream set as it stood BEFORE the
			// block — concurrent siblings can't consume each other's outputs. A
			// sequence arm's steps see earlier steps of the SAME arm (serial), so
			// each arm walks its own copy; every arm's producers become upstream
			// only after the whole block (PS-1).
			produced := map[string]bool{}
			for j := range step.Jobs {
				pj := step.Jobs[j]
				switch pj.Type {
				case "sequence":
					if len(pj.Steps) == 0 {
						addStepErr(errs, pj, "steps", "sequence arm requires at least one step")
					}
					armUp := copyBoolSet(upstream)
					validateStepSeq(pj.Steps, armUp, errs)
					for name := range armUp {
						if !upstream[name] {
							produced[name] = true
						}
					}
				case "", "job":
					if pj.Name == "" {
						addStepErr(errs, pj, "name", "parallel job requires a name")
					}
					if len(pj.Jobs) > 0 || len(pj.Steps) > 0 || pj.Pass != nil || pj.Fail != nil || pj.Condition != nil {
						addStepErr(errs, pj, "jobs", "a parallel arm must be a plain job or a sequence")
					}
					validateInputs(pj, upstream, errs)
					validateRetry(pj, errs)
					if pj.Name != "" {
						produced[pj.Name] = true
					}
				default:
					// SW is deliberately NOT allowed as a bare parallel arm in v1.
					// walkSteps' arm loop distinguishes exactly two shapes (leaf job
					// vs sequence), and widening it is a separate change; wrapping
					// the sub-workflow in a one-step sequence gives the same result
					// today with no new concurrency semantics to reason about.
					addStepErr(errs, pj, "type", "a parallel arm must be a plain job or a sequence "+
						"(wrap a workflow step in a sequence to run it as an arm)")
				}
			}
			for name := range produced {
				upstream[name] = true
			}

		case "branch":
			validateBranch(step, upstream, errs)
			// Both arms' producers are visible downstream (the skipped arm is
			// recorded with status=skipped, so its names exist in results). Walk
			// each arm with its own copy of upstream (arms are independent paths),
			// then merge what they produced back so successor steps can ref them.
			if step.Pass != nil {
				armUp := copyBoolSet(upstream)
				validateStepSeq(step.Pass.Steps, armUp, errs)
				mergeBoolSet(upstream, armUp)
			}
			if step.Fail != nil {
				armUp := copyBoolSet(upstream)
				validateStepSeq(step.Fail.Steps, armUp, errs)
				mergeBoolSet(upstream, armUp)
			}

		case stepKindWorkflow:
			// SW. A sub-workflow step needs BOTH a name (its position in the
			// parent's results map, which {fromStep} addresses) and a workflow to
			// run. Referential checks — does that workflow exist, and does the
			// reference close a cycle — live at the call site with the DB, like
			// the job-existence check.
			if step.Name == "" {
				addStepErr(errs, step, "name", "workflow step requires a name")
			}
			if strings.TrimSpace(step.Workflow) == "" {
				addStepErr(errs, step, "workflow", "workflow step requires a workflow to run")
			}
			if len(step.Jobs) > 0 || len(step.Steps) > 0 || step.Pass != nil || step.Fail != nil || step.Condition != nil {
				addStepErr(errs, step, "type", "jobs/steps/condition/pass/fail are not valid on a workflow step")
			}
			validateInputs(step, upstream, errs)
			validateRetry(step, errs)
			if step.Name != "" {
				upstream[step.Name] = true
			}

		default:
			addStepErr(errs, step, "type", "unknown step type "+strconv.Quote(step.Type)+" (want job|parallel|branch|sequence|workflow)")
		}
	}
}

// validateBranch checks a branch step's required arms and condition shape.
func validateBranch(step Step, upstream map[string]bool, errs *[]ValidationError) {
	if step.Condition == nil {
		addStepErr(errs, step, "condition", "branch step requires a condition")
	} else {
		c := step.Condition
		switch c.Type {
		case "job_status":
			// JobRef required; no field/operator/value.
		case "output_match":
			if c.Field == "" {
				addStepErr(errs, step, "condition.field", "output_match condition requires a field")
			}
			switch c.Operator {
			case "==", "!=", "contains":
			default:
				addStepErr(errs, step, "condition.operator", "output_match operator must be one of ==, !=, contains")
			}
		case "":
			addStepErr(errs, step, "condition.type", "condition requires a type (job_status|output_match)")
		default:
			addStepErr(errs, step, "condition.type", "unknown condition type "+strconv.Quote(c.Type)+" (want job_status|output_match)")
		}
		if c.JobRef == "" {
			addStepErr(errs, step, "condition.jobRef", "condition requires a jobRef")
		}
	}
	if step.Pass == nil {
		addStepErr(errs, step, "pass", "branch step requires a pass arm")
	}
	if step.Fail == nil {
		addStepErr(errs, step, "fail", "branch step requires a fail arm")
	}
}

// validateInputs checks each declared A12 input's FromStep resolves to a producer
// seen upstream of the consuming step.
func validateInputs(step Step, upstream map[string]bool, errs *[]ValidationError) {
	for key, ref := range step.Inputs {
		// Reserved-namespace guard (W4, N-D1): a step input's KEY is the env var it
		// injects into the child run, so it may not be an CRONOMICON_* name — those are
		// injector-owned references, not operator-set values.
		if envref.HasAmadeusPrefix(key) {
			addStepErr(errs, step, "inputs."+key, "input key is reserved: a workflow step may not define an CRONOMICON_* env key (these are references Cronomicon injects into runs, not values you set)")
		}
		if ref.FromStep == "" {
			addStepErr(errs, step, "inputs."+key, "input must name an upstream step (fromStep)")
			continue
		}
		if !upstream[ref.FromStep] {
			addStepErr(errs, step, "inputs."+key, "input references unknown upstream step: "+ref.FromStep)
		}
	}
}

// validateRetry rejects negative per-step retry config (WB-R1).
func validateRetry(step Step, errs *[]ValidationError) {
	if step.Retries != nil && *step.Retries < 0 {
		addStepErr(errs, step, "retries", "retries must be ≥ 0")
	}
	if step.BackoffSeconds != nil && *step.BackoffSeconds < 0 {
		addStepErr(errs, step, "backoffSeconds", "backoffSeconds must be ≥ 0")
	}
}

func addStepErr(errs *[]ValidationError, step Step, field, msg string) {
	name := step.Name
	if name == "" {
		name = step.Label
	}
	*errs = append(*errs, ValidationError{Step: name, Field: field, Message: msg})
}

func copyBoolSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	maps.Copy(out, m)
	return out
}

func mergeBoolSet(dst, src map[string]bool) {
	for k, v := range src {
		if v {
			dst[k] = true
		}
	}
}

// ─── Activity emission ────────────────────────────────────────────────────────

// ActivityParams is the activity-row payload; it now lives in the shared auditlog
// package (CC.13). Aliased here so existing workflow.ActivityParams{…} call sites
// (the engine and the execution-mount handlers) keep compiling.
type ActivityParams = auditlog.ActivityParams

// EmitActivity inserts an activity row via the single shared auditlog writer
// (CC.13). Used by both the workflow engine and the execution mount (handlers).
func EmitActivity(ctx context.Context, database *sql.DB, p ActivityParams) error {
	return auditlog.WriteActivity(ctx, database, p)
}

// InsertChangeLog writes a change_log row via the single shared auditlog writer
// (CC.13). Optional columns (target, details) are now stored as empty strings
// (CC-D2), where this writer previously wrote NULLs.
func InsertChangeLog(ctx context.Context, database *sql.DB, actor, category, action, target, details string) error {
	return auditlog.WriteChangeLog(ctx, database, actor, category, action, target, details)
}

// ─── SQL helpers ─────────────────────────────────────────────────────────────

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ─── Orphan Workflow Reaper ──────────────────────────────────────────────────

// WithShutdownWG registers a WaitGroup the background reaper joins (PP-L15).
func (e *Engine) WithShutdownWG(wg *sync.WaitGroup) *Engine {
	e.shutdownWG = wg
	return e
}

// Start runs the startup orphan sweep and starts the periodic safety-net reaper.
func (e *Engine) Start(ctx context.Context) {
	e.sweepOrphansOnStartup(ctx)

	if e.shutdownWG != nil {
		e.shutdownWG.Add(1)
	}
	go func() {
		if e.shutdownWG != nil {
			defer e.shutdownWG.Done()
		}
		e.reapOrphansLoop(ctx)
	}()
}

// sweepOrphansOnStartup reconciles EVERY workflow run still 'running' at process start.
func (e *Engine) sweepOrphansOnStartup(ctx context.Context) {
	// Query all workflow runs still 'running'
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, workflow_name, workflow_id, COALESCE(started_at,''), triggered_by
		FROM workflow_runs
		WHERE status='running'`)
	if err != nil {
		e.log.Error("workflow: query running workflows at startup failed", "error", err)
		return
	}
	defer rows.Close()

	type wfOrphan struct {
		id           string
		workflowName string
		workflowID   int64
		startedAt    string
		triggeredBy  string
	}
	var orphans []wfOrphan
	for rows.Next() {
		var o wfOrphan
		if err := rows.Scan(&o.id, &o.workflowName, &o.workflowID, &o.startedAt, &o.triggeredBy); err != nil {
			e.log.Error("workflow: scan running workflow row failed", "error", err)
			continue
		}
		orphans = append(orphans, o)
	}

	n := 0
	for _, o := range orphans {
		e.reconcileWorkflowOrphan(ctx, o, "orchestrator_lost")
		n++
	}
	if n > 0 {
		e.log.Warn("workflow: reconciled orphaned workflow runs at startup (orchestrator_lost)", "count", n)
	}
}

// reapOrphansLoop runs the periodic safety-net reaper.
func (e *Engine) reapOrphansLoop(ctx context.Context) {
	staleAfter := 24 * time.Hour
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.reapOrphansOnce(ctx, staleAfter)
		}
	}
}

// reapOrphansOnce reconciles workflow runs running past the stale window.
func (e *Engine) reapOrphansOnce(ctx context.Context, staleAfter time.Duration) int {
	cutoff := time.Now().UTC().Add(-staleAfter).Format(time.RFC3339)
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, workflow_name, workflow_id, COALESCE(started_at,''), triggered_by
		FROM workflow_runs
		WHERE status='running' AND started_at IS NOT NULL AND started_at < ?`, cutoff)
	if err != nil {
		e.log.Error("workflow: periodic query running workflows failed", "error", err)
		return 0
	}
	defer rows.Close()

	type wfOrphan struct {
		id           string
		workflowName string
		workflowID   int64
		startedAt    string
		triggeredBy  string
	}
	var orphans []wfOrphan
	for rows.Next() {
		var o wfOrphan
		if err := rows.Scan(&o.id, &o.workflowName, &o.workflowID, &o.startedAt, &o.triggeredBy); err != nil {
			e.log.Error("workflow: scan running workflow row failed", "error", err)
			continue
		}
		orphans = append(orphans, o)
	}

	n := 0
	for _, o := range orphans {
		e.reconcileWorkflowOrphan(ctx, o, "orchestrator_lost")
		n++
	}
	return n
}

// reconcileWorkflowOrphan transition a workflow run and its child runs to failure status.
func (e *Engine) reconcileWorkflowOrphan(ctx context.Context, o struct {
	id           string
	workflowName string
	workflowID   int64
	startedAt    string
	triggeredBy  string
}, reason string) {
	now := time.Now().UTC().Format(time.RFC3339)
	var durationMs int64
	if o.startedAt != "" {
		if t, err := time.Parse(time.RFC3339, o.startedAt); err == nil {
			durationMs = max(time.Since(t).Milliseconds(), 0)
		}
	}

	// Idempotently update status to failure.
	res, err := e.db.ExecContext(ctx, `
		UPDATE workflow_runs SET status = 'failure', completed_at = ?, duration_ms = ?
		WHERE id = ? AND status = 'running'
	`, now, durationMs, o.id)
	if err != nil {
		e.log.Error("workflow: reconcile update failed", "id", o.id, "error", err)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return // already reconciled or finished
	}

	e.log.Warn("workflow: reconciled orphaned workflow run", "id", o.id, "name", o.workflowName)

	// Emit workflow-end activity.
	if err := EmitActivity(ctx, e.db, ActivityParams{
		Kind:         "workflow-end",
		Outcome:      "failure",
		Actor:        o.triggeredBy,
		WorkflowName: o.workflowName,
		WorkflowID:   o.workflowID,
		TraceID:      o.id,
		DurationMs:   durationMs,
		Details:      reason,
	}); err != nil {
		e.log.Error("workflow: emit activity for orphaned workflow failed", "error", err)
	}

	// Also mark all non-terminal child runs of this workflow run as failed/skipped,
	// so they don't hang in queued/running forever.
	_, err = e.db.ExecContext(ctx, `
		UPDATE runs
		SET status = 'failure', queued_reason = ?, completed_at = ?, duration_ms = 0
		WHERE workflow_run_id = ? AND status IN ('queued', 'running')
	`, reason, now, o.id)
	if err != nil {
		e.log.Error("workflow: reconcile child runs failed", "workflow_run_id", o.id, "error", err)
	}
}

// ─── SW: sub-workflow reference validation ───────────────────────────────────

// WorkflowRefs returns every (workflow) step's referenced workflow name, so the
// write path can check existence and cycles without re-walking the graph.
func WorkflowRefs(steps []Step) []string {
	var out []string
	for _, n := range flattenWorkflowNodes(steps) {
		if w := strings.TrimSpace(n.Workflow); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// MaxWorkflowDepth is the nesting ceiling (PF-Q13).
//
// A Go constant rather than a setting, for the reason the reaction depth
// ceiling gives verbatim: this is a safety backstop, not a tuning knob, and
// exposing it invites raising it to 50 — which converts a contained runaway
// into an uncontained one.
//
// It composes with reaction.MaxDepth rather than sharing it. A sub-workflow
// inside a reaction chain consumes BOTH budgets independently, because they
// count different things: how deep the nesting goes, and how many times work
// has triggered more work. Collapsing them would make either ceiling
// unpredictable from the other's vantage point.
const MaxWorkflowDepth = 3

// DepthExceeded reports whether a child at this depth would breach the ceiling.
func DepthExceeded(childDepth int) bool { return childDepth > MaxWorkflowDepth }

// StepSourceOrder returns the A11 source precedence for resolving a step's job:
// an explicit per-step override alone, otherwise the workflow's own source
// first and the other pool second.
//
// Exported and shared (R2-3) because AUTHORIZATION MUST ASK THE SAME QUESTION
// EXECUTION WILL ANSWER. The compose-time scope checks used to resolve a step
// with `ORDER BY source LIMIT 1` — alphabetical, so 'amadeus' always won —
// while the engine resolves by this precedence. For a git-source workflow the
// two disagree, and the disagreement is a live authorization gap rather than a
// cosmetic one: the actor is checked against one job's scope and a different,
// same-named job is the one that runs. Same-name-across-sources is already
// legal today, so this is a bug now, not only after per-agency naming.
func StepSourceOrder(override, wfSource string) []string {
	if override != "" {
		return []string{override}
	}
	other := "amadeus"
	if wfSource == "amadeus" {
		other = "git"
	}
	return []string{wfSource, other}
}

// CollectStepRefs exposes the graph's distinct job references to the
// compose-time authorization checks (R2-3, widened by R2F-2), so they resolve
// each step exactly as the engine will. Without the source override a step
// carrying `jobSource: git` would be authorized against the amadeus job of the
// same name and then execute the git one; without the uid, a pinned step would
// be authorized against whichever twin the name-precedence walk reached — the
// same failure with the ambiguity moved one level down.
func CollectStepRefs(steps []Step) []StepRef { return collectStepRefs(steps) }
