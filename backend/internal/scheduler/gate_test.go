package scheduler

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// FX-C — the producer × gate conformance matrix.
//
// This is the deliverable of the band, more than the seam itself. The recurring
// defect was never "someone wrote the check wrong"; it was that nobody could see
// which checks a producer was supposed to have, so a producer missing one looked
// exactly like a producer that was complete. v1.0.0's review found the newest
// producer missing two gates. The audit after it found five more misses across
// three producers — including a machine-triggerable path that ignored an
// operator's pause entirely.
//
// So the matrix is written down as data, and a change to it has to be a
// deliberate edit with a stated reason rather than an omission. Adding a
// producer means adding a row here; if the row is wrong the tests below fail,
// and if it is missing PolicyFor returns the strictest answer rather than a
// permissive default.
//
// The behavioural halves live next to their producers (the API, promotion and
// file-watch tests), because what matters to an operator is that the PRODUCER
// refuses — not that a helper returned false. What is pinned here is the policy
// itself and the properties every producer must share.

func TestGateMatrix_EveryOriginIsClassified(t *testing.T) {
	for _, o := range []RunOrigin{
		OriginCron, OriginManual, OriginToken, OriginReaction, OriginPromotion,
		OriginPromotionAdHoc, OriginFileArrival,
	} {
		if _, ok := originPolicies[o]; !ok {
			t.Errorf("origin %q has no policy row — it would silently inherit the strict default "+
				"instead of a decision somebody made", o)
		}
	}
}

// The matrix, stated independently of the implementation. If a gate is
// deliberately relaxed for a producer, it is spelled out here AND carries a
// reason in originPolicies.
func TestGateMatrix_MatchesTheDecisionsOnRecord(t *testing.T) {
	type want struct{ pause, freeze, entryCals, cap, enabled bool }
	matrix := map[RunOrigin]want{
		// Every gate. The reference case.
		OriginCron: {pause: true, freeze: true, entryCals: true, cap: true, enabled: true},
		// FX-Q1: the click overrides pause; everything else binds.
		OriginManual: {pause: false, freeze: true, entryCals: false, cap: true, enabled: false},
		// FX-Q1: the machine does NOT inherit the click's override.
		OriginToken: {pause: true, freeze: true, entryCals: false, cap: true, enabled: false},
		// FX-Q2: entry calendars need an entry; these three have none.
		OriginReaction:    {pause: true, freeze: true, entryCals: false, cap: true, enabled: true},
		OriginPromotion:   {pause: true, freeze: true, entryCals: false, cap: true, enabled: true},
		OriginFileArrival: {pause: true, freeze: true, entryCals: false, cap: true, enabled: true},
		// FX2-B: the promotion of a HAND deferral inherits the hand's enabled
		// exemption — the accept-time and fire-time policies must agree, or the
		// system accepts a run it then guarantees will miss. Pause stays strict
		// (FX2-Q2): scheduled runs due during a pause are held; only the live
		// click (OriginManual, confirm dialog) walks through a pause.
		OriginPromotionAdHoc: {pause: true, freeze: true, entryCals: false, cap: true, enabled: false},
	}
	for origin, w := range matrix {
		p := PolicyFor(origin)
		if p.pause != w.pause {
			t.Errorf("%s: pause gate = %v, want %v", origin, p.pause, w.pause)
		}
		if p.globalFreeze != w.freeze {
			t.Errorf("%s: global freeze gate = %v, want %v", origin, p.globalFreeze, w.freeze)
		}
		if p.entryCalendars != w.entryCals {
			t.Errorf("%s: entry calendars gate = %v, want %v", origin, p.entryCalendars, w.entryCals)
		}
		if p.fleetCap != w.cap {
			t.Errorf("%s: fleet cap gate = %v, want %v", origin, p.fleetCap, w.cap)
		}
		if p.enabledCheck != w.enabled {
			t.Errorf("%s: enabled gate = %v, want %v", origin, p.enabledCheck, w.enabled)
		}
	}
}

// The invariants that hold across the whole matrix, so a future row cannot
// quietly opt out of them.
func TestGateMatrix_InvariantsHoldForEveryProducer(t *testing.T) {
	for origin, p := range originPolicies {
		// A fleet-wide freeze is fleet-wide. This is the gate whose entire purpose
		// is stopping work nobody has time to supervise, so a producer exempt from
		// it defeats the feature rather than tuning it.
		if !p.globalFreeze {
			t.Errorf("%s is exempt from the global calendar freeze — a freeze one producer "+
				"can walk through is not a freeze", origin)
		}
		// The cap is the operator's backpressure. Every producer respects it;
		// promotion and reactions apply it at the moment the run actually enters
		// the system rather than when it was queued, which is stricter, not looser.
		if !p.fleetCap {
			t.Errorf("%s is exempt from the fleet concurrency cap — one producer able to "+
				"exceed maxConcurrent makes the setting advisory", origin)
		}
		// Any exemption must justify itself in prose. An undocumented false here
		// is indistinguishable from the omissions this whole band exists to fix.
		if (!p.pause || !p.entryCalendars || !p.enabledCheck) && p.why == "" {
			t.Errorf("%s relaxes a gate with no reason recorded — that is exactly what an "+
				"accidental omission looks like", origin)
		}
	}
}

// Only ONE producer may run a paused definition, and it is the one with a human
// looking at a confirmation dialog. Pinned separately because it is the single
// deliberate asymmetry in the matrix, and the one most likely to be copied by a
// future producer that has no human in the loop.
func TestGateMatrix_OnlyAHumanClickOverridesPause(t *testing.T) {
	var overriding []RunOrigin
	for origin, p := range originPolicies {
		if !p.pause {
			overriding = append(overriding, origin)
		}
	}
	if len(overriding) != 1 || overriding[0] != OriginManual {
		t.Errorf("producers that ignore a pause = %v, want exactly [manual]. A producer with "+
			"nobody watching must not inherit the click's override — that was the token "+
			"route's bug (FX-Q1)", overriding)
	}
}

// An origin nobody has classified must fail CLOSED.
func TestGateMatrix_UnknownOriginGetsEveryGate(t *testing.T) {
	p := PolicyFor(RunOrigin("something-new"))
	if !p.pause || !p.globalFreeze || !p.entryCalendars || !p.fleetCap || !p.enabledCheck {
		t.Errorf("an unclassified origin got a relaxed policy %+v — a producer added without a "+
			"policy row must refuse loudly, not run unchecked", p)
	}
}

// Evaluate is total: it either allows or names the gate that refused. A refusal
// with no reason would reach an operator as a blank explanation.
func TestGateEvaluate_RefusalsAlwaysNameThemselves(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")

	// Clean: allowed.
	if g := Evaluate(ctx, pool, quietLog(), GateRequest{
		Origin: OriginCron, Source: "git", OwnerKind: "job", Name: "patch",
	}, nil); !g.Allowed {
		t.Fatalf("a clean job was refused by %q (%s)", g.Gate, g.Reason)
	}

	// Paused: refused, named, and explained.
	if _, err := pool.Exec(
		`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		 VALUES ('git','job','patch','op@example.com','2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("pause job: %v", err)
	}
	g := Evaluate(ctx, pool, quietLog(), GateRequest{
		Origin: OriginCron, Source: "git", OwnerKind: "job", Name: "patch",
	}, nil)
	if g.Allowed {
		t.Fatal("a paused job passed the gates")
	}
	if g.Gate == "" || g.Reason == "" {
		t.Errorf("refusal named gate=%q reason=%q — both must be set, they are what the "+
			"operator is shown", g.Gate, g.Reason)
	}

	// And the one producer allowed to ignore it does.
	if g := Evaluate(ctx, pool, quietLog(), GateRequest{
		Origin: OriginManual, Source: "git", OwnerKind: "job", Name: "patch",
	}, nil); !g.Allowed {
		t.Errorf("a human clicking Run was blocked by the pause (%s) — FX-Q1 says the click "+
			"overrides it", g.Gate)
	}
}

// ─── The wiring, not just the table ──────────────────────────────────────────
//
// A correct policy table nobody calls fixes nothing, and "nobody calls it" is
// the exact shape of the bug this band exists to close. These drive the real
// promotion path.

func parkDueRun(t *testing.T, pool *sql.DB, job string) string {
	t.Helper()
	id, err := InsertPendingRun(context.Background(), pool, "job", job, "git", "",
		timeNowUTCMinus(time.Minute), "op@example.com",
		&EnqueueParams{JobName: job, JobSource: "git", RunType: "bash",
			TriggerKind: "manual", TriggeredBy: "op@example.com"})
	if err != nil {
		t.Fatalf("park run: %v", err)
	}
	return id
}

func timeNowUTCMinus(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

func pendingState(t *testing.T, pool *sql.DB) (status, gate, missReason string) {
	t.Helper()
	if err := pool.QueryRow(
		`SELECT status, COALESCE(gate_kind,''), COALESCE(miss_reason,'') FROM pending_runs`).
		Scan(&status, &gate, &missReason); err != nil {
		t.Fatalf("read pending row: %v", err)
	}
	return
}

// §3 defect #3, the headline one: a pause applied AFTER a run was parked must
// still stop it. Delay can be arbitrarily long and these rows survive restarts,
// so checking at delivery time (which the reactor did) is not enough.
func TestPromotion_PauseAppliedAfterParkingStillHolds(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	parkDueRun(t, pool, "patch")

	if _, err := pool.Exec(`
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES ('git','job','patch','op@example.com','2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("a paused job's parked run fired %d time(s)", runs)
	}

	// Held, and VISIBLE: an unstamped hold shows up in neither the upcoming nor
	// the missed list, and cannot be cancelled from the UI.
	status, gate, _ := pendingState(t, pool)
	if status != "pending" {
		t.Fatalf("status = %q, want pending — the pause is not terminal", status)
	}
	if gate != GateHoldPrefix+"pause" {
		t.Errorf("gate_kind = %q, want %q — without the stamp the row is invisible in "+
			"the product for up to 24h", gate, GateHoldPrefix+"pause")
	}

	// Resuming releases it, and the run fires.
	if _, err := pool.Exec(`DELETE FROM paused_jobs`); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s.PromotePending(ctx)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status='queued'`).Scan(&runs)
	if runs != 1 {
		t.Errorf("queued runs after resume = %d, want 1 — the hold did not release", runs)
	}
}

// And when a held row finally expires, the reason names the gate rather than
// reporting an outage that never happened.
func TestPromotion_HeldRunExpiryNamesTheGate(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	id := parkDueRun(t, pool, "patch")
	if _, err := pool.Exec(`
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES ('git','job','patch','op@example.com','2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s.PromotePending(ctx) // stamps the hold

	if _, err := pool.Exec(`UPDATE pending_runs SET run_at = ? WHERE id = ?`,
		timeNowUTCMinus(30*time.Hour), id); err != nil {
		t.Fatalf("age row: %v", err)
	}
	s.PromotePending(ctx)

	status, _, reason := pendingState(t, pool)
	if status != "missed" {
		t.Fatalf("status = %q, want missed past the grace", status)
	}
	if !strings.Contains(reason, "pause") {
		t.Errorf("miss_reason = %q, want it to name the pause — otherwise on-call is paged "+
			"about an outage for a pause the operator applied themselves", reason)
	}
}

// FX2-B — the enabled gate at promotion is a PROVENANCE question. A hand
// deferral was accepted under the manual/token enabled-exemption ("runnable by
// hand"), so promoting it under the strict policy meant the system accepted a
// run it then guaranteed would miss: held by the disabled gate for the full 24h
// grace, then paged on-call about a run the operator deliberately scheduled.
func TestPromotion_DisabledJobAdHocDeferralStillFires(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	parkDueRun(t, pool, "patch") // an operator's Run-dialog deferral
	if _, err := pool.Exec(`UPDATE jobs SET enabled = 0 WHERE name='patch'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status='queued'`).Scan(&runs)
	if runs != 1 {
		t.Fatalf("an operator's deferred run of a disabled job fired %d time(s), want 1 — "+
			"the system accepted this run at park time (enabledCheck is off for the hand "+
			"origins) and must not promote it under a policy that guarantees it misses", runs)
	}
}

// …while a QUEUE-parked row of the same disabled job keeps the strict policy:
// disabling a job still stops its queued backlog. That was the point of the
// parent plan's promotion row and must survive the ad-hoc exemption.
func TestPromotion_DisabledJobQueueParkedRowIsHeld(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	parkDueRun(t, pool, "patch")
	// A queued fire IS a pending row with gate_kind='concurrency' (queue.go).
	if _, err := pool.Exec(`UPDATE pending_runs SET gate_kind = 'concurrency'`); err != nil {
		t.Fatalf("mark queued: %v", err)
	}
	if _, err := pool.Exec(`UPDATE jobs SET enabled = 0 WHERE name='patch'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("a DISABLED job's queue-parked fire promoted %d time(s) — disabling a job "+
			"must stop its queued backlog", runs)
	}
	// Held, and still visibly a queued fire: the 'concurrency' stamp is the
	// record that this row IS a queued fire, and holdForGate must not overwrite it.
	if status, gate, _ := pendingState(t, pool); status != "pending" || gate != "concurrency" {
		t.Errorf("status=%q gate=%q, want pending/concurrency", status, gate)
	}
}

// …and a REACTION-parked row likewise: a reaction has no hand behind it.
func TestPromotion_DisabledJobReactionParkedRowIsHeld(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	if _, err := InsertReactionPendingRun(ctx, pool, pendingReaction{
		Kind: "job", Name: "patch", Source: "git",
		RunAt: timeNowUTCMinus(time.Minute),
		Params: &EnqueueParams{JobName: "patch", JobSource: "git", RunType: "bash",
			TriggerKind: "reaction", TriggeredBy: "reactor"},
		OriginRef: "run-upstream", Depth: 1,
	}); err != nil {
		t.Fatalf("park reaction run: %v", err)
	}
	if _, err := pool.Exec(`UPDATE jobs SET enabled = 0 WHERE name='patch'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("a DISABLED job's reaction-parked run promoted %d time(s)", runs)
	}
	if status, gate, _ := pendingState(t, pool); status != "pending" || gate != GateHoldPrefix+"disabled" {
		t.Errorf("status=%q gate=%q, want pending/%s", status, gate, GateHoldPrefix+"disabled")
	}
}

// A fleet-wide freeze declared after parking holds the run too (FX-Q2).
func TestPromotion_GlobalFreezeDeclaredAfterParkingHolds(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	parkDueRun(t, pool, "patch")

	// Control: with no freeze it promotes.
	s.PromotePending(ctx)
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 1 {
		t.Fatalf("control: parked run did not promote (%d runs)", runs)
	}

	// Now with a freeze covering today, in the app zone the gate computes in.
	pool2 := mustPool(t)
	s2 := New(pool2, quietLog(), nil)
	seedQueueJob(t, pool2, "patch", "Allow")
	parkDueRun(t, pool2, "patch")
	seedGlobalFreezeToday(t, pool2, s2)
	s2.PromotePending(ctx)

	_ = pool2.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("a parked run promoted during a fleet-wide freeze (%d runs) — FX-Q2 says the "+
			"freeze is re-judged at the promotion instant", runs)
	}
	if status, gate, _ := pendingState(t, pool2); status != "pending" || gate != GateHoldPrefix+"calendar" {
		t.Errorf("status=%q gate=%q, want pending/%scalendar", status, gate, GateHoldPrefix)
	}
}

// seedGlobalFreezeToday inserts a global calendar naming today (in the zone the
// gate computes in) as a suppressed day.
func seedGlobalFreezeToday(t *testing.T, pool *sql.DB, s *Scheduler) {
	t.Helper()
	day := calendar.DayOf(time.Now(), s.location())
	// global=1 is the fleet-wide tier; a global calendar is skip-polarity by
	// construction (CAL-27 — a global `only` calendar would be a system-wide
	// outage shaped like a feature).
	if _, err := pool.Exec(
		`INSERT INTO calendars (name, source, global, created_at)
		 VALUES ('change-freeze','cronomicon',1,'2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}
	if _, err := pool.Exec(
		`INSERT INTO calendar_days (calendar_source, calendar_name, day, label)
		 VALUES ('cronomicon','change-freeze',?,'Change freeze')`, day); err != nil {
		t.Fatalf("seed calendar day: %v", err)
	}
}

// entryCalendars is documentation, not behaviour: Evaluate does not implement
// it, and only cron — which resolves its own entry bindings — sets it. If that
// stops being true, this fails rather than silently granting a producer
// calendars it never gets.
func TestGateMatrix_EntryCalendarsAreCronOnly(t *testing.T) {
	for origin, p := range originPolicies {
		if p.entryCalendars && origin != OriginCron {
			t.Errorf("%s declares entryCalendars, but Evaluate does not apply entry bindings — "+
				"it would silently get none. Implement them in Evaluate, or the table is lying.", origin)
		}
	}
}

// ─── FX-D4: the PRODUCER half of the suppression notification ────────────────
//
// The dispatcher test (internal/notify) proves a `skipped` rule matches. This
// proves something ever EMITS one — the seam-crossing half, without which the
// two halves can both pass while the feature does nothing. That is exactly how
// FX-B1's stamp shipped broken: the consumer was tested against a hand-seeded
// row and nothing drove the producer.

type captureRuns struct{ events []notify.RunEvent }

func (c *captureRuns) RunEnded(ev notify.RunEvent)   { c.events = append(c.events, ev) }
func (c *captureRuns) AlertRaised(notify.AlertEvent) {}

func TestSuppressionEmitsANotification(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	got := &captureRuns{}
	s.SetNotifier(got)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")

	if err := s.recordSuppression(ctx, EnqueueParams{
		JobName: "patch", JobSource: "git", RunType: "bash", Scope: "prod",
		ScheduleName: "nightly",
	}, skipRecord{Reason: reasonJobPaused, Mode: dedupeEpisodeReason}); err != nil {
		t.Fatalf("record suppression: %v", err)
	}
	if len(got.events) != 1 {
		t.Fatalf("suppression emitted %d notify events, want 1 — a rule on 'skipped' has been "+
			"selectable since the feature shipped and nothing could ever emit one", len(got.events))
	}
	ev := got.events[0]
	if ev.Status != "skipped" {
		t.Errorf("event status = %q, want skipped", ev.Status)
	}
	if ev.Reason != reasonJobPaused {
		t.Errorf("event reason = %q, want the suppression reason — a skip has no exit code "+
			"to explain itself", ev.Reason)
	}
	if ev.Scope != "prod" {
		t.Errorf("event scope = %q, want prod (it gates who may be told)", ev.Scope)
	}

	// A DE-DUPED repeat writes no row and must not emit either: the de-dupe
	// exists so a per-minute schedule suppressed on a holiday writes one row and
	// not 1,440, and notifications are where that volume is least affordable.
	if err := s.recordSuppression(ctx, EnqueueParams{
		JobName: "patch", JobSource: "git", RunType: "bash", Scope: "prod",
		ScheduleName: "nightly",
	}, skipRecord{Reason: reasonJobPaused, Mode: dedupeEpisodeReason}); err != nil {
		t.Fatalf("record duplicate suppression: %v", err)
	}
	if len(got.events) != 1 {
		t.Errorf("a de-duped suppression emitted another notification (%d total) — the row was "+
			"collapsed but the page was not", len(got.events))
	}
}

// FX2-D1 — the gate-hold log line fires on the TRANSITION, not on every 15s
// promotion tick. SQLite's RowsAffected counts a row rewritten to the same
// value, so the first version logged ~5,760 identical lines a day per held row
// — the exact flood holdForRecycleBin's comment promises this design avoids.
func TestGateHoldLogsOnceNotEveryTick(t *testing.T) {
	pool := mustPool(t)
	var buf bytes.Buffer
	s := New(pool, slog.New(slog.NewTextHandler(&buf, nil)), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	parkDueRun(t, pool, "patch")
	if _, err := pool.Exec(`
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES ('git','job','patch','op@example.com','2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("pause: %v", err)
	}

	for range 3 {
		s.PromotePending(ctx) // three ticks, one transition
	}
	if n := strings.Count(buf.String(), "held by an admission gate"); n != 1 {
		t.Fatalf("hold logged %d times over 3 ticks, want 1 — per-tick logging is ~5,760 "+
			"identical lines a day for one held row", n)
	}

	// A gate CHANGE is a new transition: the stamp must follow it and may log
	// again. Lift the pause and declare a fleet-wide freeze.
	if _, err := pool.Exec(`DELETE FROM paused_jobs`); err != nil {
		t.Fatal(err)
	}
	seedGlobalFreezeToday(t, pool, s)
	s.PromotePending(ctx)
	if _, gate, _ := pendingState(t, pool); gate != GateHoldPrefix+"calendar" {
		t.Errorf("gate after the pause lifted under a freeze = %q, want %s — the no-op guard "+
			"must not also block a genuine gate change", gate, GateHoldPrefix+"calendar")
	}
	if n := strings.Count(buf.String(), "held by an admission gate"); n != 2 {
		t.Errorf("hold logged %d times after a gate change, want 2 — a change of cause is "+
			"news, a retry of the same cause is not", n)
	}
}

// FX2-D2 — a WORKFLOW pending-run miss dispatches as workflow-owned.
// target_mode='job' rules match by NAME alone, so an empty OwnerKind paged
// whoever watches a same-named JOB — the cross-namespace misfire the field was
// added (FX-B3) to prevent, recommitted by the band that added it.
func TestWorkflowPendingMissAlertCarriesOwnerKind(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	got := &captureAlerts{}
	s.SetNotifier(got)
	ctx := context.Background()

	if _, err := InsertPendingRun(ctx, pool, "workflow", "deploy", "git", "",
		timeNowUTCMinus(30*time.Hour), "op@example.com", nil); err != nil {
		t.Fatalf("park workflow run: %v", err)
	}
	s.PromotePending(ctx) // past the grace → missed → alert

	if len(got.events) != 1 {
		t.Fatalf("workflow pending miss raised %d alerts, want 1", len(got.events))
	}
	ev := got.events[0]
	if ev.OwnerKind != "workflow" {
		t.Errorf("alert OwnerKind = %q, want workflow — a job-targeted rule for a same-named "+
			"job would match this event and page the wrong operator", ev.OwnerKind)
	}
	if !strings.Contains(ev.Subject, "workflow deploy") {
		t.Errorf("alert subject = %q, want the 'workflow' label its sla.go twin carries", ev.Subject)
	}

	// And the job arm keeps the empty-means-job convention, twin-checked.
	pool2 := mustPool(t)
	s2 := New(pool2, quietLog(), nil)
	got2 := &captureAlerts{}
	s2.SetNotifier(got2)
	seedQueueJob(t, pool2, "deploy", "Allow")
	if _, err := InsertPendingRun(ctx, pool2, "job", "deploy", "git", "",
		timeNowUTCMinus(30*time.Hour), "op@example.com",
		&EnqueueParams{JobName: "deploy", JobSource: "git", RunType: "bash"}); err != nil {
		t.Fatalf("park job run: %v", err)
	}
	s2.PromotePending(ctx)
	if len(got2.events) != 1 {
		t.Fatalf("job pending miss raised %d alerts, want 1", len(got2.events))
	}
	if ev := got2.events[0]; ev.OwnerKind != "" {
		t.Errorf("job-arm OwnerKind = %q, want empty (the empty-means-job convention)", ev.OwnerKind)
	}
}

// A "Missed:" marker must NOT emit here: it already notifies through
// TriggerMissedRun, which asks a different and louder question. Emitting both
// would page twice for one event.
func TestMissedMarkerDoesNotDoubleNotify(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	got := &captureRuns{}
	s.SetNotifier(got)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")

	if err := s.recordSuppression(ctx, EnqueueParams{
		JobName: "patch", JobSource: "git", RunType: "bash",
	}, skipRecord{Reason: reasonMissedFire, Mode: dedupeEpisodeReason}); err != nil {
		t.Fatalf("record missed marker: %v", err)
	}
	if len(got.events) != 0 {
		t.Errorf("a missed-fire marker emitted %d run notifications — it already alerts through "+
			"TriggerMissedRun, so this is the second page for one event", len(got.events))
	}
}

// TestReactionPendingRunStampsOwnerUID — R2-2. The reactor parks runs through
// its own INSERT, separate from InsertPendingRun's, and the two have drifted
// before. Both must stamp owner_uid or the cascade quietly falls back to the
// name arm (see internal/db/cascade_uid_test.go for why that is not enough).
func TestReactionPendingRunStampsOwnerUID(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedQueueJob(t, pool, "reacted", "Allow")
	if _, err := pool.Exec(`UPDATE jobs SET uid='uid-reacted' WHERE name='reacted' AND source='git'`); err != nil {
		t.Fatalf("set uid: %v", err)
	}

	id, err := InsertReactionPendingRun(ctx, pool, pendingReaction{
		Kind: "job", Name: "reacted", Source: "git",
		RunAt:     timeNowUTCMinus(time.Minute),
		OriginRef: "run-upstream", Depth: 1,
	})
	if err != nil {
		t.Fatalf("park reaction run: %v", err)
	}
	var uid sql.NullString
	if err := pool.QueryRow(`SELECT owner_uid FROM pending_runs WHERE id = ?`, id).Scan(&uid); err != nil {
		t.Fatalf("read owner_uid: %v", err)
	}
	if uid.String != "uid-reacted" {
		t.Errorf("reaction pending_runs.owner_uid = %v, want uid-reacted", uid)
	}
}
