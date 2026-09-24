package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// Ad-hoc run scheduling (AR) — promotion of pending_runs into real runs.
//
// A pending run is a fully-validated manual trigger parked until its run_at
// instant (migration 790). This loop owns the firing side: every tick it
// promotes due rows through the SAME enqueue seam a live trigger uses, so a
// deferred run is indistinguishable from an immediate one from dispatch onward.
//
// Semantics, decided deliberately:
//
//   - Catch-up, bounded. Unlike a cron fire (at-most-once; downtime loses it),
//     an operator who scheduled a specific run at 5pm almost certainly wants it
//     to fire on startup if the server was down at 5pm. Rows are therefore
//     promoted whenever found due — including the first pass after a restart —
//     UNLESS they are more than pendingMissGrace late, in which case they are
//     marked 'missed' and kept visible: firing a chosen instant a day late is
//     more surprising than not firing it.
//   - Cap and Forbid are judged at PROMOTION, not at scheduling. "The system is
//     busy now" says nothing about tomorrow 5pm, so the trigger path skips both
//     for deferred runs; here a blocked row simply stays pending and is retried
//     next tick until it fires or ages past the grace into 'missed'.
//   - Fired and cancelled rows are deleted (History / Change Log own those
//     trails); only 'missed' rows persist, so the table stays a work queue.

const (
	// pendingPromoteInterval is the promotion cadence. Sub-minute so a run
	// scheduled on a minute boundary fires within seconds of it, matching the
	// cron engine's perceived punctuality.
	pendingPromoteInterval = 15 * time.Second
	// pendingMissGrace bounds catch-up: a row this far past run_at is marked
	// missed instead of fired.
	pendingMissGrace = 24 * time.Hour
)

// PendingWorkflowFire is one promoted pending WORKFLOW run's dispatch envelope.
//
// RX-11 — a params struct rather than four more positional arguments. The firer
// previously took (source, name, triggeredBy) and HARDCODED TriggerKind:
// "manual" at the call site, which was harmless while ad-hoc deferral was the
// only producer and actively wrong the moment a second one existed: a
// reaction-fired workflow would have been labelled manual in History, and the
// depth ceiling would have read 0 on every hop because nothing carried the
// chain position forward.
type PendingWorkflowFire struct {
	Source      string
	Name        string
	TriggeredBy string
	// TriggerKind is manual for an AR deferral and reaction for a reactor
	// promotion. Empty is treated as manual by the engine.
	TriggerKind string
	// EnvJSON carries the AMADEUS_REACTED_TO_* stamp for a reaction; empty for
	// an ad-hoc deferral, which has no provenance to pass on.
	EnvJSON string
	// ReactionDepth / ReactedToRunID — see workflow.TriggerParams. Zero-valued
	// for every non-reaction promotion.
	ReactionDepth  int
	ReactedToRunID string
}

// PendingWorkflowFirer dispatches a promoted pending WORKFLOW run. Injected
// from main (like WorkflowFirer) so the scheduler need not import the workflow
// package; TriggeredBy is the operator who scheduled it (or "reactor"),
// preserved so the workflow run's audit trail names its cause.
type PendingWorkflowFirer func(ctx context.Context, p PendingWorkflowFire)

// SetPendingWorkflowFirer installs the pending-workflow dispatch callback.
func (s *Scheduler) SetPendingWorkflowFirer(f PendingWorkflowFirer) {
	s.mu.Lock()
	s.pendingWfFirer = f
	s.mu.Unlock()
}

// pendingLoop ticks the promotion pass. Started from Start alongside the
// backstop loop; the immediate first pass is the restart catch-up.
func (s *Scheduler) pendingLoop(ctx context.Context) {
	s.PromotePending(ctx)
	t := time.NewTicker(pendingPromoteInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.PromotePending(ctx)
		}
	}
}

// pendingRow is one due pending_runs row.
type pendingRow struct {
	id, kind, name, source string
	runAt, scheduledBy     string
	paramsJSON             sql.NullString
	// Reaction provenance (RX-1). originKind is 'reaction' for a reactor-produced
	// row and NULL for an ordinary AR deferral, which is what keeps the two
	// producers distinguishable at promotion.
	originKind, originRef sql.NullString
	originEnvJSON         sql.NullString
	reactionDepth         int
	// gateKind is why the row is parked, when it is parked for a reason other
	// than the clock: 'concurrency' (QP) or 'recycle_bin' (FX-A3).
	gateKind sql.NullString
	// scope is the parked run's scope, used for the SU-2 read filter and carried
	// onto the FX-B2 miss marker so a scoped job's miss is not globally visible.
	scope sql.NullString
	// createdAt is when the row was parked. For a GATE-QUEUED row that is the
	// instant the schedule fired (queue.go parks at fire time), which is why the
	// missed-run detector matches on it — and why FX-B1 carries it onto the
	// promoted run as scheduled_for.
	createdAt string
}

// isAdHoc reports whether this row's PROVENANCE is a person or token deferring
// a run from the Run dialog — as opposed to a queue park (gate_kind =
// 'concurrency') or a reaction (origin_kind = 'reaction'). Provenance and
// gate-state are different axes: the hold stamps ('recycle_bin', 'gate:*')
// FX-A3/FX-C write onto a waiting row say why it is CURRENTLY held, not who
// parked it, so they must read the same as no stamp at all here. Two consumers
// depend on this exact predicate — promoteOne's origin choice (FX2-B) and the
// /schedules/upcoming adHoc label (FX2-D3, via the SQL twin in
// schedules_api.go pendingRunItems) — which is why it is a method rather than
// two hand-copies that can drift.
func (p pendingRow) isAdHoc() bool {
	return p.originKind.String != "reaction" && p.gateKind.String != gateConcurrency
}

// PromotePending fires every due pending run. Exported for tests and for the
// force-reload style seams; safe to call concurrently with the ticker (each
// row's terminal write is guarded by its current status).
func (s *Scheduler) PromotePending(ctx context.Context) {
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, source, run_at, scheduled_by, params_json,
		       origin_kind, origin_ref, origin_env_json, reaction_depth, gate_kind, created_at, scope
		FROM pending_runs WHERE status = 'pending' AND run_at <= ?
		ORDER BY run_at ASC`, now.Format(time.RFC3339))
	if err != nil {
		s.log.Error("pending: query due rows", "err", err)
		return
	}
	var due []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.id, &p.kind, &p.name, &p.source, &p.runAt, &p.scheduledBy, &p.paramsJSON,
			&p.originKind, &p.originRef, &p.originEnvJSON, &p.reactionDepth, &p.gateKind, &p.createdAt, &p.scope); err == nil {
			due = append(due, p)
		}
	}
	rows.Close() // drain BEFORE the per-row writes (SQLite pool rule)

	for _, p := range due {
		s.promoteOne(ctx, p, now)
	}
}

func (s *Scheduler) promoteOne(ctx context.Context, p pendingRow, now time.Time) {
	// Too late to be the run the operator chose?
	if at, err := time.Parse(time.RFC3339, p.runAt); err != nil || now.Sub(at) > pendingMissGrace {
		reason := "malformed run_at"
		if err == nil {
			reason = "past the " + pendingMissGrace.String() + " catch-up grace"
			// FX-A3 — name the CAUSE, not just the symptom. A row held because its
			// owner sat in the recycle bin until the grace ran out is a different
			// story from one the server was simply down for, and "past the grace"
			// alone sends the operator looking for an outage that never happened.
			switch {
			case p.gateKind.String == GateRecycleBin:
				reason += " (held while its " + p.kind + " was in the recycle bin)"
			case strings.HasPrefix(p.gateKind.String, GateHoldPrefix):
				reason += " (held by the " + strings.TrimPrefix(p.gateKind.String, GateHoldPrefix) + " gate)"
			}
		}
		s.markPendingMissed(ctx, p, reason)
		return
	}

	// FX-C — every admission gate is judged NOW, at the instant the run actually
	// enters the system, not when the row was parked.
	//
	// Backpressure always was ("the system is busy now" says nothing about
	// tomorrow at 5pm). PAUSE and the global calendar FREEZE were not, and that
	// was the gap: a delay can be arbitrarily long and these rows survive
	// restarts, so a pause or a freeze declared AFTER a row was parked still has
	// to stop it. The reactor even documented the hole from its own side —
	// "promoteOne does NOT check this, so the reactor must" — but it could only
	// cover the instant of DELIVERY, which is not the instant that matters.
	//
	// A blocked row stays pending and retries next tick, exactly as the cap
	// always behaved: none of these are terminal, and all three can clear.
	//
	// FX2-B — WHICH promotion policy is a provenance question. A hand deferral
	// was accepted under the manual/token enabled-exemption and must not be
	// promoted under a policy that guarantees it misses; a queue-parked or
	// reaction-parked row keeps the strict one. See the origin table's why.
	origin := OriginPromotion
	if p.isAdHoc() {
		origin = OriginPromotionAdHoc
	}
	if g := Evaluate(ctx, s.db, s.log, GateRequest{
		Origin: origin, Source: p.source, OwnerKind: p.kind, Name: p.name,
	}, s.location()); !g.Allowed {
		// STAMPED, not just logged. A held row's run_at is already in the past, and
		// /schedules/upcoming admits a past-dated row only when a gate names it —
		// which is exactly why the QP and recycle-bin holds stamp one. A row held
		// by pause or a freeze with no stamp lands in neither the upcoming nor the
		// missed list: invisible in the product, and uncancellable, since Cancel
		// needs the pendingId only that endpoint carries.
		//
		// It also has to be stamped so the eventual expiry can name the CAUSE.
		// Without it, a run held because the operator paused the job is reported
		// 24h later as "never started: past the catch-up grace" — which pages
		// on-call about an outage that never happened, for a pause they applied
		// themselves. FX-A3 learned this for the bin; the same holds here.
		s.holdForGate(ctx, p, g)
		return
	}
	s.clearGateHold(ctx, p)

	switch p.kind {
	case "workflow":
		s.mu.Lock()
		firer := s.pendingWfFirer
		s.mu.Unlock()
		if firer == nil {
			s.log.Error("pending: no workflow firer wired; row retried", "workflow", p.name)
			return
		}
		// RX-11 — the existence check the job branch has and this one lacked.
		// The delete trigger should have cascaded a deleted workflow's pending
		// rows, but a RENAME leaves the same gap the job comment describes, and
		// the firer looks the workflow up only AFTER promoteOne has already
		// deleted the row — so a missing workflow silently consumed the pending
		// run and produced nothing. Missed, loudly, not a ghost.
		// FX-A3 — "exists" and "runnable" are different questions, and the bin is
		// the difference. A binned owner still exists, so its parked runs WAIT
		// (see the job branch below for the full reasoning).
		var wfBinned int
		if err := s.db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM workflows WHERE source = ? AND name = ?`,
			p.source, p.name).Scan(&wfBinned); err != nil {
			s.markPendingMissed(ctx, p, "workflow no longer exists")
			return
		}
		if wfBinned == 1 {
			s.holdForRecycleBin(ctx, p)
			return
		}
		s.clearRecycleBinHold(ctx, p)
		fire := PendingWorkflowFire{
			Source:      p.source,
			Name:        p.name,
			TriggeredBy: p.scheduledBy,
			TriggerKind: "manual",
		}
		if p.originKind.String == "reaction" {
			fire.TriggerKind = "reaction"
			fire.EnvJSON = p.originEnvJSON.String
			fire.ReactionDepth = p.reactionDepth
			fire.ReactedToRunID = p.originRef.String
		}
		if !s.deletePending(ctx, p.id) {
			return // raced with a cancel — the cancel wins
		}
		firer(ctx, fire)
		s.log.Info("pending: fired workflow run", "workflow", p.name, "source", p.source,
			"scheduledBy", p.scheduledBy, "triggerKind", fire.TriggerKind, "runAt", p.runAt)

	default: // job
		var params EnqueueParams
		if err := json.Unmarshal([]byte(p.paramsJSON.String), &params); err != nil {
			s.markPendingMissed(ctx, p, "unreadable frozen params: "+err.Error())
			return
		}
		// The job must still exist — the delete trigger should have cascaded,
		// but a rename leaves the same gap. Missed, loudly, not a ghost run.
		// FX-A3 — the row must survive its owner's trip through the recycle bin.
		// Binning is reversible by design, but marking the parked run `missed` is
		// terminal and no restore path revives it, so a bin-and-restore inside the
		// catch-up window silently destroyed work the operator never cancelled —
		// and said "job no longer exists", which was not true of a job sitting in
		// the bin. A binned owner therefore HOLDS the row: promotion retries each
		// tick, a restore before the grace expires fires it, and a row still binned
		// when the grace runs out expires through the ordinary path above with the
		// honest "past the catch-up grace" reason.
		var jobBinned int
		if err := s.db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM jobs WHERE source = ? AND name = ?`,
			params.jobSourceOrDefault(), params.JobName).Scan(&jobBinned); err != nil {
			s.markPendingMissed(ctx, p, "job no longer exists")
			return
		}
		if jobBinned == 1 {
			s.holdForRecycleBin(ctx, p)
			return
		}
		s.clearRecycleBinHold(ctx, p)
		// RX — restore the reaction context from the ROW, not from the frozen
		// params. The reactor writes it to dedicated columns because a workflow
		// target has no params_json to hold it, and because the delivery row that
		// also knows it is retention-pruned; reading it here keeps both target
		// kinds on one mechanism. A delayed reaction promoted after a restart
		// therefore still carries its provenance, which is the case that would
		// otherwise silently lose it.
		if p.originKind.String == "reaction" {
			params.ReactionDepth = p.reactionDepth
			params.ReactedToRunID = p.originRef.String
		}

		// S16 Forbid — judged now, like the cap. Blocked rows retry. The policy
		// rides the frozen params (Policy field, set by the trigger path) so
		// promotion needs no jobs-table join that a rename could invalidate.
		// QP: Queue re-judges the same gate. A queued row IS this retry — it is
		// due immediately and stays pending until the key clears, which is why
		// the queue needed no loop of its own.
		if cronutil.HoldsGate(params.Policy) {
			if conflict, err := CheckForbid(ctx, s.db, params.ConcurrencyKey); err == nil && conflict {
				s.log.Info("pending: deferred by concurrency policy; will retry",
					"job", p.name, "key", params.ConcurrencyKey, "policy", params.Policy)
				return
			}
		}
		// FX-B1 — carry the ORIGINAL fire instant onto the run before the pending
		// row (which is the only other record of it) is deleted.
		//
		// Only for a gate-queued row: that is the one whose created_at IS the
		// instant the schedule fired, and the one the missed-run detector would
		// otherwise lose track of. An ad-hoc deferral has no schedule expecting
		// it, so it has nothing to record and stays NULL.
		if p.gateKind.String == gateConcurrency {
			params.ScheduledFor = p.createdAt
		}
		if !s.deletePending(ctx, p.id) {
			return // raced with a cancel
		}
		traceID, err := EnqueueRunWithID(ctx, s.db, params)
		if err != nil {
			s.log.Error("pending: enqueue failed", "job", p.name, "err", err)
			return
		}
		s.log.Info("pending: fired job run", "job", p.name, "source", p.source, "trace", traceID,
			"scheduledBy", p.scheduledBy, "runAt", p.runAt)
	}
}

// GateRecycleBin marks a parked row as held because its owner is in the recycle
// bin (FX-A3). It rides the existing gate_kind column that QP added for the
// concurrency hold, because the two are the same idea: a row that is alive and
// waiting on a condition, rather than late.
//
// Exported because the read side (/schedules/upcoming) must classify on the same
// literal the write side stamps; two copies of "recycle_bin" is exactly the kind
// of drift that makes a held row invisible again.
const GateRecycleBin = "recycle_bin"

// gateConcurrency is the QP hold: a fire parked because its concurrency key is
// busy. Named here so Go-side comparisons cannot drift from the SQL that writes
// it (queue.go) or the SQL that reads it (sla.go).
const gateConcurrency = "concurrency"

// GateHoldPrefix marks a gate_kind written by an admission gate (FX-C), so the
// read side can distinguish "held by pause" from "held by a freeze" without a
// new column, and can tell either from the QP hold that predates them.
const GateHoldPrefix = "gate:"

// holdForRecycleBin stamps the hold and logs it ONCE, on the transition.
//
// The stamp is what makes the row visible: /schedules/upcoming classifies a
// parked row as missed, gate-queued, or future-dated, and a held row is none of
// those — its run_at is already past. Without a gate it showed up in neither
// list, so the run was invisible in the product for up to 24h and could not be
// cancelled from the UI, which needs the pendingId this endpoint carries. That
// is the same trap the concurrency branch was written to escape.
//
// Logging only on the transition matters too: promotion ticks every 15s, so an
// unconditional Info line is ~5,760 entries a day for one binned job.
// holdForGate stamps a hold for any admission gate that refused (FX-C), reusing
// the mechanism FX-A3 built for the recycle bin. gate_kind carries WHICH gate,
// so the read side can say what a row is waiting on rather than only that it is.
func (s *Scheduler) holdForGate(ctx context.Context, p pendingRow, g GateOutcome) {
	gate := GateHoldPrefix + g.Gate
	// FX2-D1 — the predicate refuses the no-op rewrite, so RowsAffected means
	// "transition" and the Info line fires once per hold (or gate CHANGE, e.g.
	// pause → calendar), not once per 15s tick. SQLite counts a row rewritten to
	// the same value, so without the <> arm a single held row logged ~5,760
	// identical lines a day — the flood holdForRecycleBin's comment promises
	// this design avoids. That twin guards with a bare IS NULL; this one cannot,
	// because a hold may legitimately move between gates and the stamp must
	// follow it.
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_runs SET gate_kind = ? WHERE id = ? AND status = 'pending'
		   AND (gate_kind IS NULL OR (gate_kind LIKE 'gate:%' AND gate_kind <> ?))`, gate, p.id, gate)
	if err != nil {
		s.log.Error("pending: mark gate hold", "id", p.id, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.log.Info("pending: held by an admission gate; will retry",
			"kind", p.kind, "name", p.name, "id", p.id, "gate", g.Gate, "reason", g.Reason)
	}
}

// clearGateHold releases a gate hold once the gates pass again, so a row does
// not keep reporting a pause that has been lifted.
func (s *Scheduler) clearGateHold(ctx context.Context, p pendingRow) {
	if !strings.HasPrefix(p.gateKind.String, GateHoldPrefix) {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pending_runs SET gate_kind = NULL WHERE id = ? AND status = 'pending' AND gate_kind LIKE 'gate:%'`,
		p.id); err != nil {
		s.log.Error("pending: clear gate hold", "id", p.id, "err", err)
	}
}

func (s *Scheduler) holdForRecycleBin(ctx context.Context, p pendingRow) {
	// ONLY an ungated row is stamped. A row already held by the Queue policy keeps
	// gate_kind='concurrency', because that value is not a label — it is the
	// record that this row IS a queued fire, and four separate readers depend on
	// it: the missed-run detector's parked-fire check, FX-B1's scheduled_for
	// stamp at promotion, the queue-depth count that enforces QueueCap, and the
	// upcoming projection. Overwriting it (and then clearing it to NULL on
	// restore, which is what the first version did) destroyed all four for the
	// lifetime of the row — resurrecting the very false-page defect v1.0.0 fixed.
	// A queued row that is also binned stays visibly queued, which is true and
	// costs nothing: it is waiting on both.
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_runs SET gate_kind = ? WHERE id = ? AND status = 'pending'
		   AND gate_kind IS NULL`, GateRecycleBin, p.id)
	if err != nil {
		s.log.Error("pending: mark recycle-bin hold", "id", p.id, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.log.Info("pending: owner is in the recycle bin; row held until it is restored",
			"kind", p.kind, "name", p.name, "source", p.source, "id", p.id, "runAt", p.runAt)
	}
}

// clearRecycleBinHold releases the hold when the owner comes back. Promotion
// usually deletes the row moments later, but not always — the cap or the
// concurrency gate can defer it — and a row left carrying a stale hold would
// keep reporting a recycle bin its owner is no longer in.
func (s *Scheduler) clearRecycleBinHold(ctx context.Context, p pendingRow) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_runs SET gate_kind = NULL WHERE id = ? AND status = 'pending' AND gate_kind = ?`,
		p.id, GateRecycleBin)
	if err != nil {
		s.log.Error("pending: clear recycle-bin hold", "id", p.id, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.log.Info("pending: owner restored; hold released", "kind", p.kind, "name", p.name, "id", p.id)
	}
}

// deletePending removes a pending row, returning false when it was already
// gone — the promote/cancel race resolves to whoever deletes first.
func (s *Scheduler) deletePending(ctx context.Context, id string) bool {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pending_runs WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		s.log.Error("pending: delete row", "id", id, "err", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func (s *Scheduler) markPendingMissed(ctx context.Context, p pendingRow, reason string) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_runs SET status = 'missed', miss_reason = ? WHERE id = ? AND status = 'pending'`,
		reason, p.id)
	if err != nil {
		s.log.Error("pending: mark missed", "id", p.id, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return // already terminal, or cancelled out from under us — don't double-report
	}
	s.log.Warn("pending: run missed", "kind", p.kind, "name", p.name, "runAt", p.runAt,
		"scheduledBy", p.scheduledBy, "reason", reason)
	s.reportPendingMiss(ctx, p, reason)
}

// reportPendingMiss gives an expired parked run the signal it never had (FX-B2).
//
// Until now this path wrote `status='missed'` and a log line, and that was ALL:
// no runs row, so History and analytics showed nothing; no notification, so
// nobody was told; no metric, so no dashboard moved. A scheduled fire that never
// happened produced zero evidence anywhere an operator looks — the invisible-
// failure mode this area of the code keeps having to fix. Its only surface was
// /schedules/upcoming, where it was additionally mislabelled `adHoc: true` even
// when it came from a cron entry.
//
// It routes through the SAME recording seam every other suppression uses
// (recordSkippedFire + raiseAlert), rather than a bespoke one, so the row
// de-dupes and reads like every other explained non-run.
func (s *Scheduler) reportPendingMiss(ctx context.Context, p pendingRow, reason string) {
	if p.kind != "job" {
		// Workflow misses have no runs-table equivalent to write (a workflow's
		// suppressions live in workflow_runs and recordSkippedWorkflowFire wants a
		// definition this row may no longer have). The alert below still fires,
		// which is the half that reaches a person.
		s.alertPendingMiss(ctx, p, reason)
		return
	}
	var params EnqueueParams
	if p.paramsJSON.Valid {
		_ = json.Unmarshal([]byte(p.paramsJSON.String), &params)
	}
	if params.JobName == "" {
		params.JobName, params.JobSource = p.name, p.source
	}
	// Scope from the ROW when the frozen params could not supply it — one of the
	// paths that reaches here is "unreadable frozen params", and a marker inserted
	// with a NULL scope for a scoped job is readable by everyone (the SU-2 filter
	// admits NULL as global). pending_runs.scope exists for exactly this read.
	if params.Scope == "" {
		params.Scope = p.scope.String
	}
	if params.RunType == "" {
		params.RunType = s.runTypeOf(ctx, params.jobSourceOrDefault(), params.JobName)
	}
	// The run is stamped at the instant it was MEANT to happen, not at the moment
	// the sweep noticed, so History places it where the operator expects to find it.
	// The BARE constant, not the constant plus this row's reason text.
	// dedupeEpisodeReason keys on exact string equality, so a variable tail —
	// which here can include a raw json.Unmarshal error message — would make every
	// miss its own de-dupe episode and would stop these rows collapsing with the
	// detector's own. The specific cause travels on the alert and the log line,
	// where it is read by a person rather than compared by a key.
	if err := s.recordSuppression(ctx, params, skipRecord{
		Reason: reasonMissedFire,
		Mode:   dedupeEpisodeReason,
		At:     p.runAt,
	}); err != nil {
		s.log.Error("pending: record missed run", "id", p.id, "err", err)
	}
	s.alertPendingMiss(ctx, p, reason)
}

func (s *Scheduler) alertPendingMiss(ctx context.Context, p pendingRow, reason string) {
	at := p.runAt
	if t, err := time.Parse(time.RFC3339, p.runAt); err == nil {
		at = t.In(s.location()).Format(time.RFC3339)
	}
	// FX2-D2 — OwnerKind and the subject label, mirroring the sla.go twin
	// (recordMissedWorkflowFire). target_mode='job' rules match by NAME alone;
	// without OwnerKind a workflow's miss dispatched as job-owned and paged
	// whoever watches a same-named JOB — the exact cross-namespace misfire the
	// field was added to prevent, recommitted by the band that added it.
	ownerKind, subject := "", "[Cronomicon] "+p.name+" did not run"
	if p.kind == "workflow" {
		ownerKind = "workflow"
		subject = "[Cronomicon] workflow " + p.name + " did not run"
	}
	s.raiseAlert(ctx, notify.AlertEvent{
		Trigger:   notify.TriggerMissedRun,
		JobName:   p.name,
		OwnerKind: ownerKind,
		Scope:     p.scope.String,
		Subject:   subject,
		Detail: "A run scheduled for " + at + " by " + p.scheduledBy +
			" never started: " + reason + ".",
	}, "Missed run", "missed")
}

// pendingReaction is the reaction context a parked run must carry (RX-1/RX-10).
//
// For a JOB target the frozen params_json could hold the env and depth, but for
// a WORKFLOW target params_json is NULL — and the delivery and the promotion are
// separated by delay_seconds plus possibly a restart, so nothing may ride in
// memory or in the retention-pruned delivery row. Persisting all of it on the
// pending row is what makes a delayed reaction survive a restart with its
// provenance intact.
type pendingReaction struct {
	Kind      string // job | workflow (Phase B fires jobs only)
	Name      string
	Source    string
	Scope     string
	RunAt     string
	Params    *EnqueueParams
	OriginRef string // the upstream run id — becomes runs.reacted_to_run_id
	Depth     int    // the DOWNSTREAM depth (upstream + 1)
	EnvJSON   string // the AMADEUS_REACTED_TO_* stamp
}

// InsertReactionPendingRun parks a reaction-resolved run. Distinct from
// InsertPendingRun rather than an extra parameter on it, because the AR path's
// call sites should not have to pass four zero values for a feature they know
// nothing about — and because `scheduled_by` differs in kind: a reaction has no
// operator, so it records the reactor.
func InsertReactionPendingRun(ctx context.Context, database *sql.DB, p pendingReaction) (string, error) {
	id := db.NewTraceID()
	var paramsJSON, scopeVal, envVal any
	if p.Params != nil {
		b, err := json.Marshal(p.Params)
		if err != nil {
			return "", err
		}
		paramsJSON = string(b)
	}
	if p.Scope != "" {
		scopeVal = p.Scope
	}
	if p.EnvJSON != "" {
		envVal = p.EnvJSON
	}
	_, err := database.ExecContext(ctx, `
		INSERT INTO pending_runs
			(id, kind, name, source, scope, run_at, scheduled_by, created_at, status, params_json,
			 origin_kind, origin_ref, reaction_depth, origin_env_json, owner_uid)
		VALUES (?, ?, ?, ?, ?, ?, 'reactor', ?, 'pending', ?, 'reaction', ?, ?, ?,
			CASE ?
			  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
			  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
			END)`,
		id, p.Kind, p.Name, p.Source, scopeVal, p.RunAt,
		time.Now().UTC().Format(time.RFC3339), paramsJSON,
		p.OriginRef, p.Depth, envVal,
		p.Kind, p.Name, p.Source, p.Name, p.Source)
	if err != nil {
		return "", err
	}
	return id, nil
}

// InsertPendingRun parks a validated trigger until runAt. For jobs, params is
// the frozen EnqueueParams; for workflows pass nil. Returns the pending row id.
func InsertPendingRun(ctx context.Context, database *sql.DB, kind, name, source, scope, runAt, scheduledBy string, params *EnqueueParams) (string, error) {
	id := db.NewTraceID()
	var paramsJSON any
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return "", err
		}
		paramsJSON = string(b)
	}
	var scopeVal any
	if scope != "" {
		scopeVal = scope
	}
	_, err := database.ExecContext(ctx, `
		INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, status, params_json,
			owner_uid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?,
			CASE ?
			  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
			  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
			END)`,
		id, kind, name, source, scopeVal, runAt, scheduledBy,
		time.Now().UTC().Format(time.RFC3339), paramsJSON,
		kind, name, source, name, source)
	if err != nil {
		return "", err
	}
	return id, nil
}
