package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/envmerge"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/reaction"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// The reactor (the reactions-update plan, RX-9 — Phase B).
//
// A reaction fires when ANOTHER definition finishes. This loop is the producer:
// it observes terminal rows, decides whether each matching reaction should fire,
// and resolves the ones that should into pending_runs rows. It adds a PRODUCER,
// not a dispatch path — the concurrency cap, the Forbid policy and the
// definition-existence check are judged once, at promotion, by promoteOne.
//
// # Why polling, and not a hook in the terminal writers
//
// There are six job-side terminal writers and no shared guard between them.
// notify.Dispatcher.RunEnded is the closest thing to a consolidated seam, but
// the kill route and the workflow-skip path do not call it — so a reactor built
// on that seam would silently miss every stopped run, which is precisely the
// class of event `on_outcome: stopped` exists to catch. Polling reads the
// tables that ARE the truth.
//
// # Why a lookback window and not a strict watermark
//
// completed_at is written by several uncoordinated writers with no
// serialisation, so it is NOT monotonic across concurrent runs: a strict cursor
// would silently drop an event that landed with an earlier stamp after the
// cursor moved past it. An overlap window plus an idempotent delivery key is
// correct under overlap, restart and clock skew alike. This is the same
// discipline CAL-8 needed for suppression de-duping.
//
// # Both sides are kind-agnostic (all four quadrants, RX-11)
//
// job→job, job→workflow, workflow→job and workflow→workflow are ONE primitive,
// not four, because both halves were already modelled: the owner side reuses
// definition_schedules' tuple and the target side is pending_runs' kind. There
// is no job-specific or workflow-specific branch in the matching or the
// suppression stack — the only asymmetry is in READING the event (two tables,
// two normalisers) and in what a target freezes: a job target freezes its full
// EnqueueParams now, while a workflow target freezes nothing because the engine
// resolves its own steps at trigger time.

const (
	// reactionScanInterval matches the pending promotion cadence: the reactor is
	// the same kind of thing, and a reaction should feel as prompt as a deferred
	// run.
	reactionScanInterval = 15 * time.Second

	// reactionLookback is the overlap window. Generous on purpose — the cost of
	// re-examining an event is one INSERT OR IGNORE that does nothing, while the
	// cost of missing one is a cascade that never happens and leaves no trace.
	reactionLookback = 15 * time.Minute

	// reactionMissGrace bounds catch-up, mirroring pendingMissGrace. Without it,
	// a Monday-morning restart after a weekend outage discharges an entire
	// weekend of reactions at once — the single worst thing this feature could
	// do to a Government install.
	reactionMissGrace = 24 * time.Hour

	settingsKeyReactionCursor = "reactionCursor"
)

// reactionLoop ticks the reactor. Started from Start alongside pendingLoop.
func (s *Scheduler) reactionLoop(ctx context.Context) {
	s.ScanReactions(ctx)
	t := time.NewTicker(reactionScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScanReactions(ctx)
		}
	}
}

// srcEvent is one observed terminal completion.
type srcEvent struct {
	kind        string // job | workflow
	source      string
	name        string
	runID       string
	outcome     reaction.Outcome
	completedAt time.Time
	depth       int
	// inWorkflow is true for a job run that belongs to a workflow (§2.5). Such a
	// run emits no job reaction unless the reaction opts in, because otherwise
	// the parent workflow grows fan-out that appears nowhere in its step graph.
	inWorkflow bool
}

// reactionRow is one authored reaction, as matched against an event.
type reactionRow struct {
	ownerSource, ownerKind, ownerName, name string
	// ownerUID is the reacting definition's identity (R2-5): what the delivery
	// claim de-dupes on and what a job fire resolves its target by — the name
	// pair may now mean a sibling.
	ownerUID string
	onOutcome                               string
	delaySeconds                            int
	minIntervalSeconds                      int
	includeWorkflowChildren                 bool
	enabled                                 bool
}

// ScanReactions runs one reactor pass. Exported for tests, which drive it
// directly rather than waiting on the ticker.
func (s *Scheduler) ScanReactions(ctx context.Context) {
	now := time.Now().UTC()

	cursor, ok := s.reactionCursor(ctx)
	if !ok {
		// First pass ever (fresh install, or the upgrade that created the table).
		// Anchor the cursor at NOW and scan nothing: a reaction fires on the NEXT
		// matching completion and never retroactively on history (§2.1). Without
		// this, the first boot after upgrade would consider every historical run
		// in the retention window and write an 'expired' delivery for each — a
		// pointless write storm whose only visible effect is alarm.
		s.writeReactionCursor(ctx, now)
		return
	}

	events := s.collectEvents(ctx, cursor.Add(-reactionLookback))
	if len(events) == 0 {
		return
	}
	// Oldest first, ACROSS both tables — each query is ordered, but the two are
	// concatenated. Without this the rate brake's "which event won" depends on
	// DB row order, so after a backlog (a restart, or a burst inside one tick) a
	// reaction with a min_interval would fire for an arbitrary upstream and
	// stamp AMADEUS_REACTED_TO_RUN_ID with it. Chronological order makes the
	// survivor the earliest event and every later one a recorded suppression,
	// which is at least explicable.
	sort.Slice(events, func(i, j int) bool {
		if events[i].completedAt.Equal(events[j].completedAt) {
			return events[i].runID < events[j].runID // stable under equal stamps
		}
		return events[i].completedAt.Before(events[j].completedAt)
	})

	newest := cursor
	for _, ev := range events {
		if ev.completedAt.After(newest) {
			newest = ev.completedAt
		}
		s.deliverEvent(ctx, ev, now)
	}

	// The cursor never goes backwards. Scanning resumes a full lookback behind
	// it, which is what makes the overlap window an overlap rather than a gap.
	if newest.After(cursor) {
		s.writeReactionCursor(ctx, newest)
	}
}

// collectEvents reads every terminal completion at or after `since` from both
// run tables and normalises each to an outcome.
//
// Rows are fully drained into memory BEFORE any per-row write happens. That is
// not a style choice: this pool must never hold an open cursor across a write on
// the same connection, and deliverEvent writes.
func (s *Scheduler) collectEvents(ctx context.Context, since time.Time) []srcEvent {
	sinceStr := since.Format(time.RFC3339)
	var out []srcEvent

	jobRows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(job_source,'git'), job_name, status, completed_at,
		       reaction_depth, workflow_run_id
		  FROM runs
		 WHERE completed_at IS NOT NULL AND completed_at >= ?
		   AND status IN ('success','warning','failure','killed')
		   -- kind='ssh-test' rows are connectivity probes, not job executions.
		   -- They carry a synthetic job_name ("SSH Test — <target>") that a
		   -- reaction could technically be authored against; excluding them here
		   -- keeps a diagnostic from ever being a trigger.
		   AND kind = 'job'
		 ORDER BY completed_at`, sinceStr)
	if err != nil {
		s.log.Error("reactor: scan runs", "err", err)
	} else {
		for jobRows.Next() {
			var id, source, name, status, completedAt string
			var depth int
			var wfRunID sql.NullString
			if err := jobRows.Scan(&id, &source, &name, &status, &completedAt, &depth, &wfRunID); err != nil {
				continue
			}
			outcome, isEvent := reaction.NormalizeJob(status)
			if !isEvent {
				continue
			}
			ts, perr := time.Parse(time.RFC3339, completedAt)
			if perr != nil {
				continue
			}
			out = append(out, srcEvent{
				kind: "job", source: source, name: name, runID: id,
				outcome: outcome, completedAt: ts.UTC(), depth: depth,
				inWorkflow: wfRunID.Valid && wfRunID.String != "",
			})
		}
		jobRows.Close()
	}

	wfRows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(workflow_source,'git'), workflow_name, status, cancelled,
		       completed_at, reaction_depth
		  FROM workflow_runs
		 WHERE completed_at IS NOT NULL AND completed_at >= ?
		   AND status IN ('success','warning','failure','killed')
		 ORDER BY completed_at`, sinceStr)
	if err != nil {
		s.log.Error("reactor: scan workflow_runs", "err", err)
		return out
	}
	for wfRows.Next() {
		var id, source, name, status, completedAt string
		var cancelled, depth int
		if err := wfRows.Scan(&id, &source, &name, &status, &cancelled, &completedAt, &depth); err != nil {
			continue
		}
		outcome, isEvent := reaction.NormalizeWorkflow(status, cancelled == 1)
		if !isEvent {
			continue
		}
		ts, perr := time.Parse(time.RFC3339, completedAt)
		if perr != nil {
			continue
		}
		out = append(out, srcEvent{
			kind: "workflow", source: source, name: name, runID: id,
			outcome: outcome, completedAt: ts.UTC(), depth: depth,
		})
	}
	wfRows.Close()
	return out
}

// deliverEvent matches one event against every reaction watching it, and
// decides each independently.
func (s *Scheduler) deliverEvent(ctx context.Context, ev srcEvent, now time.Time) {
	// Disabled reactions are matched too, deliberately, so the delivery log can
	// record suppressed_disabled rather than staying silent: "we saw this and did
	// nothing because you switched it off" is a decision worth being able to
	// query when somebody asks why the cascade stopped.
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner_source, owner_kind, owner_name, name, on_outcome,
		       delay_seconds, min_interval_seconds, include_workflow_children, enabled,
		       COALESCE(owner_uid,'')
		  FROM reactions
		 WHERE on_kind = ? AND on_source = ? AND on_name = ?`,
		ev.kind, ev.source, ev.name)
	if err != nil {
		s.log.Error("reactor: match reactions", "kind", ev.kind, "name", ev.name, "err", err)
		return
	}
	var matched []reactionRow
	for rows.Next() {
		var r reactionRow
		var includeChildren, enabled int
		if err := rows.Scan(&r.ownerSource, &r.ownerKind, &r.ownerName, &r.name, &r.onOutcome,
			&r.delaySeconds, &r.minIntervalSeconds, &includeChildren, &enabled, &r.ownerUID); err != nil {
			continue
		}
		r.includeWorkflowChildren = includeChildren == 1
		r.enabled = enabled == 1
		if !reaction.Matches(r.onOutcome, ev.outcome) {
			continue
		}
		// §2.5 — a workflow's child run does not emit a job reaction unless this
		// reaction explicitly opted in.
		if ev.inWorkflow && !r.includeWorkflowChildren {
			continue
		}
		matched = append(matched, r)
	}
	rows.Close() // drain BEFORE the per-row writes (SQLite pool rule)

	for _, r := range matched {
		s.deliverOne(ctx, ev, r, now)
	}
}

// deliverOne records and decides a single (reaction, event) pair.
func (s *Scheduler) deliverOne(ctx context.Context, ev srcEvent, r reactionRow, now time.Time) {
	// The delivery row is written BEFORE the verdict, not after, so the record of
	// "we considered this event" survives a crash mid-decision. A suppressed
	// delivery is a decision that was made, not an event that was lost — the
	// distinction CAL-8 had to fix retroactively for skipped fires.
	//
	// This insert is also the idempotency gate. RowsAffected == 0 means another
	// pass (or another process) already owns this pair, so we stop: the overlap
	// window means we see most events several times, and without this every
	// reaction would fire once per scan.
	//
	// The claim value is 'pending', NOT a provisional 'fired'. A provisional
	// 'fired' would be found by the rate brake's own last-fired lookup a few
	// lines below — so every reaction carrying a min_interval would suppress
	// itself on its very first event — and a crash before the verdict would
	// leave the row claiming a fire that never happened.
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO reaction_deliveries
			(owner_source, owner_kind, owner_name, name, src_kind, src_run_id,
			 outcome, result, delivered_at, owner_uid)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, NULLIF(?, ''))`,
		r.ownerSource, r.ownerKind, r.ownerName, r.name, ev.kind, ev.runID,
		string(ev.outcome), now.Format(time.RFC3339), r.ownerUID)
	if err != nil {
		s.log.Error("reactor: claim delivery", "reaction", r.name, "owner", r.ownerName, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // already delivered
	}

	verdict, detail := s.reactionVerdict(ctx, ev, r, now)
	if verdict != "fired" {
		s.finishDelivery(ctx, r, ev, verdict, detail, "")
		// A depth-ceiling hit is not an ordinary suppression: it means a cycle
		// that static detection did not see, which is a defect in the graph or
		// in the checker rather than a policy working as intended. So it gets
		// both a WARN and — §2.8/RX-Q6 — an ACTIVITY row, because the delivery
		// table is not somewhere a human looks.
		if verdict == "suppressed_depth" {
			s.log.Warn("reactor: reaction chain hit the depth ceiling — a cycle static detection missed",
				"owner", r.ownerName, "reaction", r.name, "upstream", ev.name, "detail", detail)
			s.emitDepthCeilingActivity(ctx, ev, r, detail, now)
			return
		}
		s.log.Info("reactor: reaction suppressed",
			"owner", r.ownerName, "reaction", r.name, "upstream", ev.name,
			"outcome", string(ev.outcome), "result", verdict, "detail", detail)
		return
	}

	pendingID, err := s.fireReaction(ctx, ev, r, now)
	if err != nil {
		// "Decided to fire, could not" is its own verdict, not a leftover claim.
		// It is reachable in ordinary operation — the reacting job was deleted or
		// renamed, or RA-24 refused it for unbound department-owned credentials —
		// so leaving the row at its 'pending' claim value would make it
		// indistinguishable from a crash before the decision, which is the exact
		// ambiguity 'pending' exists to prevent.
		//
		// Deliberately NOT retried: the decision was made, and re-deciding it on
		// the next scan would re-run the whole suppression stack against a
		// different clock and a different calendar day.
		s.finishDelivery(ctx, r, ev, "error", err.Error(), "")
		s.log.Error("reactor: fire reaction", "owner", r.ownerName, "reaction", r.name, "err", err)
		return
	}
	s.finishDelivery(ctx, r, ev, "fired", detail, pendingID)
	s.log.Info("reactor: reaction fired",
		"owner", r.ownerName, "reaction", r.name, "upstream", ev.name,
		"outcome", string(ev.outcome), "pending", pendingID, "delaySeconds", r.delaySeconds)
}

// reactionVerdict runs the suppression stack. Returns the delivery `result` and
// a human detail for it; "fired" means nothing vetoed.
//
// Order is deliberate: the event-level question (is this too old to act on?)
// comes first, then the reaction's own switches, then the environment's
// (paused, frozen), then the runaway brakes. Each veto is recorded, so a
// reaction that did not fire can always say which gate stopped it.
func (s *Scheduler) reactionVerdict(ctx context.Context, ev srcEvent, r reactionRow, now time.Time) (string, string) {
	// The reaction's own switch comes first because it is the PERMANENT answer:
	// a disabled reaction would not have fired for a fresh event either, so
	// reporting "expired" for it would name a gate that was not the real reason
	// and send an operator looking at timing rather than at the switch they
	// turned off.
	if !r.enabled {
		return "suppressed_disabled", "the reaction is disabled"
	}
	if now.Sub(ev.completedAt) > reactionMissGrace {
		return "expired", fmt.Sprintf("upstream finished %s ago, past the %s catch-up grace",
			now.Sub(ev.completedAt).Truncate(time.Minute), reactionMissGrace)
	}
	if enabled, known := s.definitionEnabled(ctx, r.ownerSource, r.ownerKind, r.ownerName); known && !enabled {
		// jobs.enabled / workflows.enabled is a separate lever from paused_jobs,
		// and a disabled definition must no more react than a paused one.
		return "suppressed_disabled", "the reacting definition is disabled"
	}
	if s.isPaused(ctx, r.ownerSource, r.ownerKind, r.ownerName) {
		// promoteOne does NOT check this, so the reactor must. A paused
		// definition not reacting is not arguable.
		return "suppressed_paused", "the reacting definition is paused"
	}
	if v, day, ok := s.globalCalendarVerdict(ctx); ok && v.Suppressed {
		// RX-20 — GLOBAL tier only. A fleet-wide freeze must stop a cascade: a
		// chain that runs THROUGH a freeze day is worse than a schedule that
		// does, because nobody authored it to happen today. Entry-level bindings
		// are excluded because they are a property of a schedule entry's clock,
		// and a reaction has no clock — there is no entry to consult.
		//
		// The event is dropped, not deferred. Firing on "the next non-frozen day"
		// would invent an instant nobody authored, detached by days from the
		// event it reacts to, and a reaction is edge-triggered — a stale edge is
		// worse than a missed one.
		d := fmt.Sprintf("global calendar %q covers %s", v.Calendar, day)
		if v.Label != "" {
			d = fmt.Sprintf("global calendar %q covers %s (%s)", v.Calendar, day, v.Label)
		}
		return "suppressed_calendar", d
	}
	if reaction.DepthExceeded(ev.depth) {
		return "suppressed_depth", fmt.Sprintf(
			"reaction chain would reach depth %d, at the ceiling of %d", ev.depth+1, reaction.MaxDepth)
	}
	if r.minIntervalSeconds > 0 {
		if last, ok := s.lastFiredAt(ctx, r); ok {
			if elapsed := now.Sub(last); elapsed < time.Duration(r.minIntervalSeconds)*time.Second {
				// RX-21 — dropped, not deferred. Unlike a cap deferral the event
				// has already been consumed; there is no instant to retry at.
				return "suppressed_rate", fmt.Sprintf(
					"last fired %s ago, inside the %ds minimum interval",
					elapsed.Truncate(time.Second), r.minIntervalSeconds)
			}
		}
	}
	return "fired", ""
}

// fireReaction resolves a decided reaction into a pending_runs row.
func (s *Scheduler) fireReaction(ctx context.Context, ev srcEvent, r reactionRow, now time.Time) (string, error) {
	stamp := reactionEnvStamp(ev)
	stampJSON, _ := json.Marshal(stamp)
	runAt := now.Add(time.Duration(r.delaySeconds) * time.Second)

	row := pendingReaction{
		Kind:      r.ownerKind,
		Name:      r.ownerName,
		Source:    r.ownerSource,
		RunAt:     runAt.Format(time.RFC3339),
		OriginRef: ev.runID,
		Depth:     ev.depth + 1,
		EnvJSON:   string(stampJSON),
	}

	if r.ownerKind == "job" {
		// A job target freezes its full EnqueueParams now (RX-25), so promotion
		// re-judges only what genuinely belongs at that instant — the cap and
		// Forbid — rather than re-resolving the definition.
		params, err := s.buildReactionJobParams(ctx, r.ownerSource, r.ownerName, r.ownerUID, stamp)
		if err != nil {
			return "", err
		}
		row.Params = params
		row.Scope = params.Scope
	} else {
		// A workflow target freezes nothing: the engine resolves its own steps at
		// trigger time, exactly as a cron or ad-hoc workflow fire does, and a
		// workflow definition carries no scope of its own (scope is a property of
		// the RUN — workflow_runs.scope — not of the definition). So the pending
		// row is left scope-less, matching the ad-hoc workflow deferral byte for
		// byte, and promotion re-checks existence at the instant it fires.
		//
		// The existence check here is still worth its query: it turns "a reaction
		// pointed at a workflow that no longer exists" into a recorded `error`
		// delivery naming the workflow, instead of a pending row that survives
		// until promotion and is marked missed there with the trail one step
		// further from the cause.
		var one int
		if err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM workflows WHERE source = ? AND name = ? AND deleted_at IS NULL`,
			r.ownerSource, r.ownerName).Scan(&one); err != nil {
			return "", fmt.Errorf("resolve reacting workflow %q: %w", r.ownerName, err)
		}
	}

	return InsertReactionPendingRun(ctx, s.db, row)
}

// reactionEnvStamp is the downstream run's provenance in its own environment
// (RX-10).
//
// This is the ONLY data that crosses a reaction edge. There is no output piping:
// workflows have no single outputs blob, so cross-entity piping does not
// generalise, and A12 `inputs` stays the workflow-internal mechanism it already
// is. The stamp is also the one thing a manual run cannot reproduce, which is
// why §4 keeps "run as a reaction to…" open as a future Run-dialog mode.
func reactionEnvStamp(ev srcEvent) map[string]string {
	return map[string]string{
		"AMADEUS_REACTED_TO_KIND":    ev.kind,
		"AMADEUS_REACTED_TO_SOURCE":  ev.source,
		"AMADEUS_REACTED_TO_NAME":    ev.name,
		"AMADEUS_REACTED_TO_RUN_ID":  ev.runID,
		"AMADEUS_REACTED_TO_OUTCOME": string(ev.outcome),
	}
}

// buildReactionJobParams assembles the frozen EnqueueParams for a
// reaction-fired job (RX-25).
//
// This mirrors fire()'s field assembly deliberately and in the same order. The
// seam matters: a reaction-fired job either inherits every gate and every
// resolved field a cron fire gets — targeting, identity, agencies, executor
// precedence, the frozen Policy that lets promoteOne re-judge Forbid — or it
// silently diverges into a second, subtly different way to run a job. The gates
// fire() applies that are NOT applied here (pause, calendar, cap, Forbid) are
// not missing: pause and calendar are the reactor's own suppression stack above,
// and cap and Forbid are judged at promotion, which is the whole reason a
// reaction resolves into a pending run rather than straight into a run.
func (s *Scheduler) buildReactionJobParams(ctx context.Context, source, jobName, ownerUID string, stamp map[string]string) (*EnqueueParams, error) {
	var (
		runType, scope, policy, concKey sql.NullString
		jobEnv, targetHost              sql.NullString
		sshUser, sshCred                sql.NullString
		scriptRef                       sql.NullString
		jobUID                          sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT run_type, scope, concurrency_policy, concurrency_key,
		       env_json, target_host, ssh_user, ssh_credential, script_ref, uid
		  FROM jobs
		 WHERE CASE WHEN ? != '' THEN uid = ? ELSE name = ? AND source = ? END
		   AND deleted_at IS NULL`, ownerUID, ownerUID, jobName, source).
		Scan(&runType, &scope, &policy, &concKey, &jobEnv, &targetHost, &sshUser, &sshCred, &scriptRef, &jobUID)
	if err != nil {
		// Includes ErrNoRows: a reaction whose owner was deleted or renamed. The
		// delivery stays recorded; there is simply nothing to fire.
		return nil, fmt.Errorf("resolve reacting job %q: %w", jobName, err)
	}

	if !execspec.IdentityCapableRunType(runType.String) {
		sshUser.String, sshCred.String = "", ""
	}

	// PP-L8 — only Forbid runs carry a concurrency key, so the partial unique
	// index fires only for them.
	//
	// The default MUST be source-qualified, exactly as the cron path
	// (scheduler.go) and the manual trigger (api/execution_mount.go) build it.
	// A bare job name here would be a different string from the one every other
	// producer writes, and Forbid is enforced by comparing that string: a
	// reaction-fired run of a Forbid job with no explicit key would not see a
	// cron fire's active run, nor collide with it on uq_runs_active_concurrency,
	// and two runs of a job whose whole point is never to overlap would run
	// together. This is precisely the "silently diverges into a second way to
	// run a job" failure RX-25 exists to prevent, and it is invisible until it
	// matters.
	// QP widened this from Forbid to HoldsGate for the same RX-25 reason the
	// comment above gives: a Queue job's reaction-fired run must carry the key
	// too, or it neither queues behind a cron fire nor collides with one, and
	// the policy silently becomes Allow on this one path.
	effectiveConcKey := ""
	if cronutil.HoldsGate(policy.String) {
		effectiveConcKey = cronutil.ConcurrencyKey(concKey.String, jobUID.String, source, jobName)
	}

	scopeAgencies, _ := execspec.ScopeAgencies(ctx, s.db, scope.String)

	// RA-24 — the same unbound-reference refusal the scheduled path applies. A
	// reaction fire has no human watching it either, so a run that resolves no
	// credentials and is claimable by no departmental runner must not be
	// enqueued. Unlike fire() this does not record a skipped run: the delivery
	// row already carries the trail, and inventing a run to say "this did not
	// run" would double-count the event in History.
	owners := runref.RunOwners(source, jobName, jobUID.String, scriptRef.String)
	blocked, berr := runref.UnboundRunBlocked(ctx, s.db, owners, scope.String, scopeAgencies)
	if berr != nil {
		return nil, fmt.Errorf("check unbound references for %q: %w", jobName, berr)
	}
	if len(blocked) > 0 {
		return nil, fmt.Errorf("reacting job %q consumes department-owned credentials unbound at this scope: %s",
			jobName, runref.UnboundRefusal(blocked))
	}
	// KB — same posture for a key-bound job that resolves to the ssh executor:
	// the delivery row carries the refusal, no run row is invented.
	executor := ResolveExecutor(ctx, s.db, source, jobName, runType.String)
	keys, kerr := runref.KeyBindingsOnSSH(ctx, s.db, owners, executor)
	if kerr != nil {
		return nil, fmt.Errorf("check key bindings for %q: %w", jobName, kerr)
	}
	if len(keys) > 0 {
		return nil, fmt.Errorf("reacting job %q: %s", jobName, runref.KeyBindingRefusal(keys))
	}

	stampJSON, _ := json.Marshal(stamp)
	return &EnqueueParams{
		JobName:        jobName,
		JobSource:      source,
		JobUID:         jobUID.String,
		RunType:        runType.String,
		Scope:          scope.String,
		TargetHost:     targetHost.String,
		TriggerKind:    "reaction",
		TriggeredBy:    "reactor",
		ConcurrencyKey: effectiveConcKey,
		// The reaction stamp is the OUTERMOST env layer, so a job cannot
		// accidentally shadow its own provenance with a same-named variable.
		EnvJSON:       envmerge.MergeJSON(jobEnv.String, string(stampJSON)),
		Executor:      executor,
		SSHUser:       sshUser.String,
		SSHCredential: sshCred.String,
		AgenciesJSON:  execspec.MarshalAgencies(scopeAgencies),
		// Carried so promoteOne can re-judge Forbid without a jobs-table join
		// that a rename could invalidate.
		Policy: policy.String,
	}, nil
}

// ── small readers ──────────────────────────────────────────────────────────

// definitionEnabled reads jobs.enabled / workflows.enabled. `known` is false
// when the definition does not exist, which the caller treats as "not a reason
// to suppress" — a missing definition fails later, at promotion, with a message
// about the definition rather than about a flag.
func (s *Scheduler) definitionEnabled(ctx context.Context, source, kind, name string) (enabled, known bool) {
	q := `SELECT enabled FROM jobs WHERE source = ? AND name = ? AND deleted_at IS NULL`
	if kind == "workflow" {
		q = `SELECT enabled FROM workflows WHERE source = ? AND name = ? AND deleted_at IS NULL`
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, source, name).Scan(&n); err != nil {
		return false, false
	}
	return n == 1, true
}

// globalCalendarVerdict evaluates the GLOBAL calendar tier for today.
//
// No new storage and no second day-rule implementation: calendar.Load already
// unions every global calendar unconditionally, so Evaluate with empty bindings
// yields exactly the global-tier verdict. Calling the same resolver the
// scheduler calls is what keeps fire() and the reactor from drifting into two
// answers about what today is.
//
// Failure is OPEN, matching calendarVerdict: a DB error yields "not suppressed"
// plus a log line, because converting a transient SQLite hiccup into a silent
// fleet-wide halt of all cascades is the worse failure.
func (s *Scheduler) globalCalendarVerdict(ctx context.Context) (calendar.Verdict, string, bool) {
	loc := s.location()
	day := calendar.DayOf(time.Now(), loc)
	sets, err := calendar.Load(ctx, s.db, nil)
	if err != nil {
		s.log.Error("reactor: load calendars — reacting without calendar suppression", "err", err)
		return calendar.Verdict{}, day, false
	}
	return calendar.Evaluate(sets, nil, nil, day), day, true
}

// lastFiredAt returns when this reaction last actually fired (RX-21's anchor).
func (s *Scheduler) lastFiredAt(ctx context.Context, r reactionRow) (time.Time, bool) {
	var ts string
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(delivered_at) FROM reaction_deliveries
		 WHERE owner_source = ? AND owner_kind = ? AND owner_name = ? AND name = ?
		   AND result = 'fired'`,
		r.ownerSource, r.ownerKind, r.ownerName, r.name).Scan(&ts)
	if err != nil || ts == "" {
		return time.Time{}, false
	}
	t, perr := time.Parse(time.RFC3339, ts)
	if perr != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// finishDelivery stamps the verdict onto the claimed delivery row.
func (s *Scheduler) finishDelivery(ctx context.Context, r reactionRow, ev srcEvent, result, detail, pendingID string) {
	var detailVal, pendingVal any
	if detail != "" {
		detailVal = detail
	}
	if pendingID != "" {
		pendingVal = pendingID
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE reaction_deliveries
		   SET result = ?, detail = ?, pending_run_id = ?
		 WHERE owner_source = ? AND owner_kind = ? AND owner_name = ? AND name = ?
		   AND src_kind = ? AND src_run_id = ?`,
		result, detailVal, pendingVal,
		r.ownerSource, r.ownerKind, r.ownerName, r.name, ev.kind, ev.runID); err != nil {
		s.log.Error("reactor: finish delivery", "reaction", r.name, "err", err)
	}
}

// emitDepthCeilingActivity surfaces a depth-ceiling hit in the Activity feed
// (§2.8 / RX-Q6). The delivery row records the verdict; this row is the half a
// human actually looks at, because a ceiling hit means a cycle static detection
// missed — a defect in the graph or in the checker, not a policy working.
//
// # Why kind='config' rather than a new 'reaction' kind
//
// The plan implies a new activity kind. It should NOT be one, and the reason is
// concrete rather than aesthetic: `activity.kind` is a closed CHECK, and
// auditlog's streamedActivityKinds map drops any kind absent from it before the
// event reaches audit.log. A new kind would write to the database, render in the
// UI, and be silently invisible to every SIEM — while the CSV export, which is
// kind-blind, would still contain it. The two audit surfaces disagreeing is the
// worst possible symptom for an audit feature.
//
// `config` is defined by exclusion — "the events change_log cannot carry" — and
// already carries machine-authored runtime events with no configuration edit
// behind them (runner drain, keyscan, resync, token mint). The reactions
// authoring API already writes `config` rows with Category "Reactions", so this
// joins one vocabulary instead of splitting the feature across two kinds, and it
// needs no migration, no OpenAPI change and no regenerated types.
//
// Exactly one row per (reaction, upstream run) comes for free: the caller is
// already past the INSERT OR IGNORE claim on reaction_deliveries, whose primary
// key is that pair, so a re-scan of the same event cannot re-emit.
func (s *Scheduler) emitDepthCeilingActivity(ctx context.Context, ev srcEvent, r reactionRow, detail string, now time.Time) {
	p := auditlog.ActivityParams{
		// Shares the instant the delivery row was stamped with, so History
		// cannot invert the order of the pair.
		At: now.Format(time.RFC3339),
		// Not 'failure': nothing failed, a brake engaged. 'failure' would render
		// as "Failed" and read as a run that went wrong.
		Kind:     "config",
		Outcome:  "warning",
		Actor:    "reactor",
		Category: "Reactions",
		Action:   "Depth ceiling",
		Target:   r.ownerKind + ":" + r.ownerName,
		TraceID:  ev.runID,
		Summary: fmt.Sprintf("reaction %q on %s %q hit the depth ceiling and did not fire — %s",
			r.name, ev.kind, ev.name, detail),
		Details: detail,
	}
	// The Activity card titles itself from jobName || workflowName, so naming
	// the reacting definition is what stops the row rendering as "config" twice.
	if r.ownerKind == "workflow" {
		p.WorkflowName = r.ownerName
	} else {
		p.JobName = r.ownerName
	}
	if err := auditlog.WriteActivity(ctx, s.db, p); err != nil {
		s.log.Error("reactor: emit depth-ceiling activity", "owner", r.ownerName, "reaction", r.name, "err", err)
	}
}

// reactionCursor reads the scan watermark. ok=false means "never scanned".
func (s *Scheduler) reactionCursor(ctx context.Context) (time.Time, bool) {
	var val sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, settingsKeyReactionCursor).Scan(&val); err != nil {
		return time.Time{}, false
	}
	if !val.Valid || val.String == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, val.String)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (s *Scheduler) writeReactionCursor(ctx context.Context, t time.Time) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingsKeyReactionCursor, t.UTC().Format(time.RFC3339)); err != nil {
		s.log.Error("reactor: persist cursor", "err", err)
	}
}
