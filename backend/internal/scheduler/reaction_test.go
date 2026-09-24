package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// RX Phase B — the reactor (the reactions-update plan §3).
//
// These tests exercise the loop against a real SQLite schema rather than the
// resolver in isolation (internal/reaction covers the pure outcome table). What
// is pinned here is everything that can only go wrong in the wiring: idempotency
// under an overlapping scan window, the suppression stack's order and its
// audit rows, the catch-up grace, and the fact that a reaction resolves into a
// pending run carrying its provenance rather than straight into a run.

func ctxb() context.Context { return context.Background() }

// seedReaction writes one authored reaction. Phase B has no API or YAML surface
// (that is RX-12/RX-13 in Phase C), so tests author directly — which is also
// what the reactor will read once those surfaces exist.
func seedReaction(t *testing.T, pool *sql.DB, owner, name, onName, onOutcome string, opts ...func(*map[string]any)) {
	t.Helper()
	f := map[string]any{
		"delay_seconds": 0, "min_interval_seconds": 0,
		"include_workflow_children": 0, "enabled": 1, "on_kind": "job",
	}
	for _, o := range opts {
		o(&f)
	}
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome,
		                       delay_seconds, min_interval_seconds,
		                       include_workflow_children, enabled, owner_uid)
		VALUES ('git','job',?,?,'git',?,?,?,?,?,?,?,
			(SELECT uid FROM jobs WHERE name = ? AND source = 'git'))`,
		owner, name, f["on_kind"], onName, onOutcome,
		f["delay_seconds"], f["min_interval_seconds"],
		f["include_workflow_children"], f["enabled"], owner); err != nil {
		t.Fatalf("seed reaction %s/%s: %v", owner, name, err)
	}
}

func withEnabled(v int) func(*map[string]any) {
	return func(m *map[string]any) { (*m)["enabled"] = v }
}
func withOnKind(k string) func(*map[string]any) {
	return func(m *map[string]any) { (*m)["on_kind"] = k }
}
func withMinInterval(s int) func(*map[string]any) {
	return func(m *map[string]any) { (*m)["min_interval_seconds"] = s }
}
func withChildren() func(*map[string]any) {
	return func(m *map[string]any) { (*m)["include_workflow_children"] = 1 }
}
func withDelay(s int) func(*map[string]any) {
	return func(m *map[string]any) { (*m)["delay_seconds"] = s }
}

// seedFinishedRun writes a terminal job run `ago` in the past.
func seedFinishedRun(t *testing.T, pool *sql.DB, id, job, status string, ago time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-ago).Format(time.RFC3339)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by,
		                  trigger_kind, completed_at, created_at)
		VALUES (?, ?, 'git', 'bash', ?, 't', 'scheduled', ?, ?)`,
		id, job, status, ts, ts); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
}

// primeCursor anchors the reactor so the next scan actually looks at events. The
// first pass ever deliberately scans nothing, so every test that wants a
// reaction to fire must get past it.
func primeCursor(t *testing.T, s *Scheduler) {
	t.Helper()
	s.ScanReactions(ctxb()) // anchors the cursor at "now"
	// Wind the cursor back so the seeded events fall inside the window.
	s.writeReactionCursor(ctxb(), time.Now().UTC().Add(-time.Hour))
}

func deliveries(t *testing.T, pool *sql.DB, owner string) []struct{ Result, Detail string } {
	t.Helper()
	rows, err := pool.QueryContext(ctxb(), `
		SELECT result, COALESCE(detail,'') FROM reaction_deliveries WHERE owner_name = ?`, owner)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	defer rows.Close()
	var out []struct{ Result, Detail string }
	for rows.Next() {
		var r, d string
		_ = rows.Scan(&r, &d)
		out = append(out, struct{ Result, Detail string }{r, d})
	}
	return out
}

func countPending(t *testing.T, pool *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRowContext(ctxb(),
		`SELECT COUNT(*) FROM pending_runs WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return n
}

// ── the happy path ─────────────────────────────────────────────────────────

// A matching completion resolves into a pending run carrying its provenance.
// This is the whole feature in one test.
func TestReactionFiresIntoAPendingRun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	seedJobRow(t, pool, "downstream", 1)
	seedReaction(t, pool, "downstream", "on-upstream", "upstream", "success")
	seedFinishedRun(t, pool, "r1", "upstream", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "downstream"); n != 1 {
		t.Fatalf("pending runs for downstream = %d, want 1", n)
	}
	var originKind, originRef, envJSON sql.NullString
	var depth int
	if err := pool.QueryRowContext(ctxb(), `
		SELECT origin_kind, origin_ref, reaction_depth, origin_env_json
		  FROM pending_runs WHERE name = 'downstream'`).
		Scan(&originKind, &originRef, &depth, &envJSON); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if originKind.String != "reaction" {
		t.Errorf("origin_kind = %q, want reaction", originKind.String)
	}
	if originRef.String != "r1" {
		t.Errorf("origin_ref = %q, want the upstream run id r1 — this is the durable because-of link", originRef.String)
	}
	if depth != 1 {
		t.Errorf("reaction_depth = %d, want 1 (upstream 0 + 1)", depth)
	}
	// RX-10 — the stamp is the only data that crosses a reaction edge.
	for _, want := range []string{"AMADEUS_REACTED_TO_NAME", "upstream", "AMADEUS_REACTED_TO_OUTCOME", "success"} {
		if !contains(envJSON.String, want) {
			t.Errorf("origin_env_json %q missing %q", envJSON.String, want)
		}
	}
	d := deliveries(t, pool, "downstream")
	if len(d) != 1 || d[0].Result != "fired" {
		t.Errorf("deliveries = %+v, want one 'fired'", d)
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }

// The single most important negative in the feature: an unclassified stop does
// NOT satisfy a failure reaction. Without this, cancelling a deploy because you
// spotted a problem fires the rollback automation while you are hands-on.
func TestStoppedDoesNotSatisfyAFailureReaction(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	seedJobRow(t, pool, "rollback", 1)
	seedReaction(t, pool, "rollback", "on-fail", "upstream", "failure")
	seedFinishedRun(t, pool, "r-killed", "upstream", "killed", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "rollback"); n != 0 {
		t.Errorf("an unclassified stop fired a FAILURE reaction (%d pending) — a deliberate human "+
			"intervention must not trigger the automated failure response", n)
	}
	if len(deliveries(t, pool, "rollback")) != 0 {
		t.Error("a non-matching outcome should not even produce a delivery row")
	}
}

// Phase A's payoff reaching reactions: a stop RECORDED AS a success satisfies a
// success reaction, because the disposition is folded into runs.status.
func TestDispositionedStopSatisfiesItsDisposition(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	seedJobRow(t, pool, "downstream", 1)
	seedReaction(t, pool, "downstream", "on-ok", "upstream", "success")
	// An operator stopped it and recorded it as done.
	seedFinishedRun(t, pool, "r-stopok", "upstream", "success", time.Minute)
	if _, err := pool.ExecContext(ctxb(),
		`UPDATE runs SET killed_by='ops@example.com' WHERE id='r-stopok'`); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "downstream"); n != 1 {
		t.Errorf("a stop recorded as SUCCESS did not satisfy on_outcome=success (%d pending); "+
			"killed_by must not override the recorded status", n)
	}
}

// `any` matches all three outcomes and never a run that did not happen.
func TestOnOutcomeAny(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{"success", 1}, {"warning", 1}, {"failure", 1}, {"killed", 1},
		{"skipped", 0}, // CAL-8 suppression: nothing ran
	} {
		t.Run(tc.status, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			seedJobRow(t, pool, "up", 1)
			seedJobRow(t, pool, "down", 1)
			seedReaction(t, pool, "down", "on-any", "up", "any")
			seedFinishedRun(t, pool, "r-"+tc.status, "up", tc.status, time.Minute)

			primeCursor(t, s)
			s.ScanReactions(ctxb())

			if n := countPending(t, pool, "down"); n != tc.want {
				t.Errorf("status %q under on_outcome=any: pending = %d, want %d", tc.status, n, tc.want)
			}
		})
	}
}

// ── idempotency ────────────────────────────────────────────────────────────

// The overlap window means most events are seen several times. Without the
// delivery key, a reaction would fire once per scan — a 15-second cascade.
func TestRepeatedScansFireOnce(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	primeCursor(t, s)
	for range 5 {
		s.ScanReactions(ctxb())
	}
	if n := countPending(t, pool, "down"); n != 1 {
		t.Errorf("five overlapping scans produced %d pending runs, want 1", n)
	}
}

// A restart re-reads the cursor from the settings row. Simulated by building a
// second Scheduler over the same pool: it must not re-fire what the first
// already delivered.
func TestRestartDoesNotRefire(t *testing.T) {
	pool := mustPool(t)
	s1 := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	primeCursor(t, s1)
	s1.ScanReactions(ctxb())

	s2 := New(pool, quietLog(), nil) // "restart"
	s2.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 1 {
		t.Errorf("after a restart the reaction fired again (%d pending, want 1) — the delivery "+
			"key, not the cursor, is what makes this safe", n)
	}
}

// The very first pass on a fresh install must anchor the cursor and scan
// NOTHING. Otherwise the upgrade that creates the table discharges every
// historical completion in the retention window at once.
func TestFirstScanIsNotRetroactive(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r-old", "up", "success", 2*time.Minute)

	s.ScanReactions(ctxb()) // first pass ever

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("the first scan fired %d reactions on pre-existing history, want 0 — a new "+
			"reaction fires on the NEXT completion, never retroactively", n)
	}
	if len(deliveries(t, pool, "down")) != 0 {
		t.Error("the first scan should not even write delivery rows for history")
	}
}

// ── the suppression stack ──────────────────────────────────────────────────

func TestSuppressionStack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, pool *sql.DB)
		wantResult string
	}{
		{
			name:       "disabled reaction",
			setup:      func(t *testing.T, pool *sql.DB) { seedReactionDisabled(t, pool) },
			wantResult: "suppressed_disabled",
		},
		{
			name: "disabled owner definition",
			setup: func(t *testing.T, pool *sql.DB) {
				seedReaction(t, pool, "down", "r", "up", "success")
				if _, err := pool.ExecContext(ctxb(), `UPDATE jobs SET enabled = 0 WHERE name='down'`); err != nil {
					t.Fatal(err)
				}
			},
			wantResult: "suppressed_disabled",
		},
		{
			name: "paused owner definition",
			setup: func(t *testing.T, pool *sql.DB) {
				seedReaction(t, pool, "down", "r", "up", "success")
				if _, err := pool.ExecContext(ctxb(),
					`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
					 VALUES ('git','job','down','ops@example.com','2026-01-01T00:00:00Z')`); err != nil {
					t.Fatal(err)
				}
			},
			wantResult: "suppressed_paused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			seedJobRow(t, pool, "up", 1)
			seedJobRow(t, pool, "down", 1)
			tc.setup(t, pool)
			seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

			primeCursor(t, s)
			s.ScanReactions(ctxb())

			if n := countPending(t, pool, "down"); n != 0 {
				t.Errorf("%s: fired anyway (%d pending)", tc.name, n)
			}
			d := deliveries(t, pool, "down")
			if len(d) != 1 || d[0].Result != tc.wantResult {
				t.Fatalf("%s: deliveries = %+v, want one %q — a suppressed reaction must never "+
					"be silent", tc.name, d, tc.wantResult)
			}
			if d[0].Detail == "" {
				t.Errorf("%s: suppression recorded no detail; the row has to say WHY", tc.name)
			}
		})
	}
}

func seedReactionDisabled(t *testing.T, pool *sql.DB) {
	t.Helper()
	seedReaction(t, pool, "down", "r", "up", "success", withEnabled(0))
}

// RX-20 — a GLOBAL calendar covering today suppresses the cascade and names the
// day. A fleet-wide freeze that a chain runs straight through is worse than a
// schedule that does, because nobody authored the chain to happen today.
func TestGlobalCalendarSuppressesAReaction(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedCal(t, pool, "change-freeze", true, true, map[string]string{today(s): "Year-end freeze"})
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("a global freeze did not stop the cascade (%d pending)", n)
	}
	d := deliveries(t, pool, "down")
	if len(d) != 1 || d[0].Result != "suppressed_calendar" {
		t.Fatalf("deliveries = %+v, want one suppressed_calendar", d)
	}
	// CAL-8's principle: the day must be nameable afterwards, not just logged.
	if !contains(d[0].Detail, "change-freeze") || !contains(d[0].Detail, "Year-end freeze") {
		t.Errorf("detail = %q, want the calendar name and the day's label", d[0].Detail)
	}
}

// An ENTRY-level binding on the owner's own schedule must NOT suppress a
// reaction: that binding is a property of a schedule entry's clock, and a
// reaction has no clock. Only the global tier applies (RX-Q3).
func TestEntryLevelCalendarBindingDoesNotSuppressAReaction(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	// Non-global calendar, bound to the reacting job's own schedule entry.
	seedCal(t, pool, "holidays", false, false, map[string]string{today(s): "A holiday"})
	bindSchedule(t, pool, "job", "down", "nightly", `["holidays"]`, "")
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 1 {
		t.Errorf("an entry-level binding suppressed a reaction (%d pending, want 1); only the "+
			"GLOBAL tier applies — a reaction has no schedule entry to consult", n)
	}
}

// RX-Q6 — the depth ceiling, the backstop for cycles static detection cannot
// see. At the ceiling the delivery records suppressed_depth and nothing fires.
func TestDepthCeilingStopsARunawayChain(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r-deep", "up", "success", time.Minute)
	// The upstream run is already at the ceiling's edge.
	if _, err := pool.ExecContext(ctxb(),
		`UPDATE runs SET reaction_depth = 4 WHERE id='r-deep'`); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("a chain at the depth ceiling still fired (%d pending)", n)
	}
	d := deliveries(t, pool, "down")
	if len(d) != 1 || d[0].Result != "suppressed_depth" {
		t.Errorf("deliveries = %+v, want one suppressed_depth", d)
	}
}

// RX-21 — the rate brake DROPS rather than defers. Unlike a cap deferral the
// event has already been consumed; there is no instant to retry at.
func TestRateBrakeDropsRatherThanDefers(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success", withMinInterval(3600))
	seedFinishedRun(t, pool, "r1", "up", "success", 3*time.Minute)
	seedFinishedRun(t, pool, "r2", "up", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 1 {
		t.Errorf("pending = %d, want 1 — the first event fires, the second is inside the interval", n)
	}
	var rate int
	if err := pool.QueryRowContext(ctxb(),
		`SELECT COUNT(*) FROM reaction_deliveries WHERE result = 'suppressed_rate'`).Scan(&rate); err != nil {
		t.Fatal(err)
	}
	if rate != 1 {
		t.Errorf("suppressed_rate deliveries = %d, want 1", rate)
	}
	// And it stays dropped: a later scan must not resurrect the consumed event.
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "down"); n != 1 {
		t.Errorf("a rate-blocked event was retried on a later scan (%d pending, want 1); it is "+
			"dropped, not deferred", n)
	}
}

// Bounded catch-up. Without it a Monday-morning restart after a weekend outage
// discharges an entire weekend of reactions at once.
func TestEventsPastTheGraceExpireRatherThanFire(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r-stale", "up", "success", 48*time.Hour)

	s.ScanReactions(ctxb())
	s.writeReactionCursor(ctxb(), time.Now().UTC().Add(-72*time.Hour))
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("a two-day-old completion fired (%d pending); the catch-up grace must stop it", n)
	}
	d := deliveries(t, pool, "down")
	if len(d) != 1 || d[0].Result != "expired" {
		t.Errorf("deliveries = %+v, want one 'expired' — the event is recorded as considered, "+
			"not silently dropped", d)
	}
}

// §2.5 — a job run inside a workflow emits no job reaction by default, or the
// parent workflow grows fan-out that appears nowhere in its step graph.
func TestWorkflowChildDoesNotEmitByDefault(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO workflow_runs (id, workflow_name, status, triggered_by, trigger_kind, created_at)
		VALUES ('wfr','nightly','success','t','scheduled','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	seedFinishedRun(t, pool, "r-child", "up", "success", time.Minute)
	if _, err := pool.ExecContext(ctxb(),
		`UPDATE runs SET workflow_run_id='wfr' WHERE id='r-child'`); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("a workflow's child run emitted a job reaction (%d pending) without opt-in", n)
	}
}

func TestWorkflowChildEmitsWhenOptedIn(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success", withChildren())
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO workflow_runs (id, workflow_name, status, triggered_by, trigger_kind, created_at)
		VALUES ('wfr','nightly','success','t','scheduled','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	seedFinishedRun(t, pool, "r-child", "up", "success", time.Minute)
	if _, err := pool.ExecContext(ctxb(),
		`UPDATE runs SET workflow_run_id='wfr' WHERE id='r-child'`); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 1 {
		t.Errorf("include_workflow_children did not take effect (%d pending, want 1)", n)
	}
}

// A cancelled workflow finalising SUCCESS (the cancel landed during the final
// step, so the walk never observed it) emits `stopped`, not `success`. Reading
// the status there would cascade a success that happened only because the
// cancel was a moment too late.
func TestCancelledWorkflowEmitsStoppedNotSuccess(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "down-ok", 1)
	seedJobRow(t, pool, "down-stop", 1)
	seedReaction(t, pool, "down-ok", "r", "nightly", "success", withOnKind("workflow"))
	seedReaction(t, pool, "down-stop", "r", "nightly", "stopped", withOnKind("workflow"))
	ts := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, cancelled,
		                           triggered_by, trigger_kind, completed_at, created_at)
		VALUES ('wfr','nightly','git','success',1,'t','scheduled',?,?)`, ts, ts); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down-ok"); n != 0 {
		t.Errorf("a CANCELLED workflow satisfied on_outcome=success (%d pending) — the cancelled "+
			"flag must win over a status that only reads success because the cancel was too late", n)
	}
	if n := countPending(t, pool, "down-stop"); n != 1 {
		t.Errorf("a cancelled workflow did not satisfy on_outcome=stopped (%d pending, want 1)", n)
	}
}

// The delay lands on run_at, not on the delivery: the reaction is decided now
// and fires later, which is what lets promoteOne judge the cap at the instant
// the run actually enters the system.
func TestDelayDefersTheRunNotTheDecision(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success", withDelay(600))
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	var runAt string
	if err := pool.QueryRowContext(ctxb(),
		`SELECT run_at FROM pending_runs WHERE name='down'`).Scan(&runAt); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	at, err := time.Parse(time.RFC3339, runAt)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(at); d < 8*time.Minute {
		t.Errorf("run_at is %s away, want ~10m — the delay must defer the RUN", d)
	}
	// The decision was still made now: the delivery is already recorded 'fired'.
	if d := deliveries(t, pool, "down"); len(d) != 1 || d[0].Result != "fired" {
		t.Errorf("deliveries = %+v, want one 'fired' at decision time", d)
	}
}

// ── RX-25: the enqueue-params seam ─────────────────────────────────────────

// A reaction-fired job must be gate-for-gate and field-for-field the same run a
// cron fire of that job would produce, except for its provenance.
//
// This is the test the plan calls for by name, and the reason RX-25 is a work
// item rather than an implementation detail: the reactor assembles
// EnqueueParams itself, so it either inherits everything fire() resolves —
// targeting, identity, agencies, executor precedence, the frozen Policy that
// lets promotion re-judge Forbid — or it silently becomes a second, subtly
// different way to run a job. Divergence here would be invisible until a
// reaction-fired run targeted the wrong host or ran as the wrong user.
func TestReactionParamsMatchACronFire(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	// A job with every resolvable field set to something non-default.
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, concurrency_key,
		                  env_json, target_host, ssh_user, ssh_credential, executor, enabled, synced_at)VALUES ('uid-'||'rich', 'rich','git','bash','prod','Forbid','rich-key','{"A":"1"}','host-7','deploy','cred-label','runner',1,'t')`); err != nil {
		t.Fatal(err)
	}

	got, err := s.buildReactionJobParams(ctxb(), "git", "rich", "", map[string]string{
		"AMADEUS_REACTED_TO_NAME": "upstream",
	})
	if err != nil {
		t.Fatalf("buildReactionJobParams: %v", err)
	}

	if got.RunType != "bash" || got.Scope != "prod" {
		t.Errorf("run type/scope = %q/%q, want bash/prod", got.RunType, got.Scope)
	}
	// TG-1 — the definition's single-host pin must reach the run. Before that
	// fix only the manual path delivered it, and a pinned job's automated fires
	// fanned out across the WHOLE scope.
	if got.TargetHost != "host-7" {
		t.Errorf("target host = %q, want host-7 — a pinned job must not fan out across its scope", got.TargetHost)
	}
	// CA-10 — the job-spec connect-as identity, for an identity-capable type.
	if got.SSHUser != "deploy" || got.SSHCredential != "cred-label" {
		t.Errorf("identity = %q/%q, want deploy/cred-label", got.SSHUser, got.SSHCredential)
	}
	// PP-L8 — a Forbid job carries its key so the partial unique index applies.
	if got.ConcurrencyKey != "rich-key" {
		t.Errorf("concurrency key = %q, want rich-key", got.ConcurrencyKey)
	}
	// The frozen policy is what lets promoteOne re-judge Forbid without a
	// jobs-table join a rename could invalidate.
	if got.Policy != "Forbid" {
		t.Errorf("policy = %q, want Forbid — promotion re-judges Forbid from this", got.Policy)
	}
	// R5.1 executor precedence: the job spec wins.
	if got.Executor != "runner" {
		t.Errorf("executor = %q, want runner (job spec wins)", got.Executor)
	}
	// The job's own env survives, with the reaction stamp layered on top.
	if !contains(got.EnvJSON, `"A":"1"`) {
		t.Errorf("env %q lost the job's own env", got.EnvJSON)
	}
	if !contains(got.EnvJSON, "AMADEUS_REACTED_TO_NAME") {
		t.Errorf("env %q lost the reaction stamp", got.EnvJSON)
	}
	// The provenance that makes it a reaction rather than a cron fire.
	if got.TriggerKind != "reaction" || got.TriggeredBy != "reactor" {
		t.Errorf("provenance = %q/%q, want reaction/reactor", got.TriggerKind, got.TriggeredBy)
	}
}

// A non-Forbid job carries NO concurrency key, matching fire(): the partial
// unique index must fire only for Forbid runs, or Allow-policy reactions would
// start colliding with each other.
func TestReactionParamsOmitConcurrencyKeyForAllowPolicy(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "plain", 1) // concurrency_policy 'Allow'

	got, err := s.buildReactionJobParams(ctxb(), "git", "plain", "", nil)
	if err != nil {
		t.Fatalf("buildReactionJobParams: %v", err)
	}
	if got.ConcurrencyKey != "" {
		t.Errorf("concurrency key = %q for an Allow-policy job, want empty", got.ConcurrencyKey)
	}
}

// The reactor produces a pending run; the EXISTING promotion loop turns it into
// a real run and threads the provenance onto it. Together these are the whole
// path, and the join between them is where a workflow target would lose its
// context if it rode in memory instead of on the row.
func TestPromotionThreadsReactionProvenanceOntoTheRun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r-src", "up", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "down"); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}

	// A fresh Scheduler promotes it — the "survives a restart" shape.
	New(pool, quietLog(), nil).PromotePending(ctxb())

	var triggerKind, triggeredBy string
	var depth int
	var reactedTo, envJSON sql.NullString
	if err := pool.QueryRowContext(ctxb(), `
		SELECT trigger_kind, triggered_by, reaction_depth, reacted_to_run_id, env_json
		  FROM runs WHERE job_name = 'down'`).
		Scan(&triggerKind, &triggeredBy, &depth, &reactedTo, &envJSON); err != nil {
		t.Fatalf("read promoted run: %v", err)
	}
	if triggerKind != "reaction" {
		t.Errorf("trigger_kind = %q, want reaction — History cannot answer \"why did this run?\" without it", triggerKind)
	}
	if triggeredBy != "reactor" {
		t.Errorf("triggered_by = %q, want reactor", triggeredBy)
	}
	if depth != 1 {
		t.Errorf("reaction_depth = %d, want 1 — the runaway backstop must survive promotion", depth)
	}
	if reactedTo.String != "r-src" {
		t.Errorf("reacted_to_run_id = %q, want r-src — this is the durable because-of link, and it "+
			"must outlive the retention-pruned delivery log", reactedTo.String)
	}
	if !contains(envJSON.String, "AMADEUS_REACTED_TO_RUN_ID") {
		t.Errorf("env_json %q lost the reaction stamp across promotion", envJSON.String)
	}
	// The pending row is consumed, not left to fire again.
	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("pending rows after promotion = %d, want 0", n)
	}
}

// A run whose completed_at predates the cursor but whose row only becomes
// visible AFTER a scan has passed that point is the exact case the lookback
// window exists for — completed_at is written by several uncoordinated writers
// and is not monotonic across concurrent runs, so a strict watermark would drop
// this event permanently and leave no trace that it had.
func TestLateArrivingEventInsideTheLookbackIsStillDelivered(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")

	// A scan happens and advances the cursor past T.
	seedFinishedRun(t, pool, "r-first", "up", "success", 2*time.Minute)
	primeCursor(t, s)
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "down"); n != 1 {
		t.Fatalf("setup: pending = %d, want 1", n)
	}

	// Only NOW does a run stamped EARLIER than the last one become visible.
	seedFinishedRun(t, pool, "r-late", "up", "success", 5*time.Minute)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 2 {
		t.Errorf("pending = %d, want 2 — an event stamped before the cursor but inserted after "+
			"it moved must still be delivered; that is what the overlap window is for", n)
	}
}

// The mirror image: an event older than the lookback window is genuinely gone.
// Pinned not because it is desirable but because it is the window's stated
// limit, and a future change to reactionLookback should have to look at this.
func TestEventOlderThanTheLookbackIsNotDelivered(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")

	seedFinishedRun(t, pool, "r-recent", "up", "success", time.Minute)
	primeCursor(t, s)
	s.ScanReactions(ctxb())

	// Older than reactionLookback (15m) behind the cursor the scan just set.
	seedFinishedRun(t, pool, "r-ancient", "up", "success", 45*time.Minute)
	s.ScanReactions(ctxb())

	var delivered int
	if err := pool.QueryRowContext(ctxb(),
		`SELECT COUNT(*) FROM reaction_deliveries WHERE src_run_id = 'r-ancient'`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Errorf("an event %v behind the cursor was delivered; the window is %v",
			45*time.Minute, reactionLookback)
	}
}

// The Forbid key must be SOURCE-QUALIFIED, exactly as the cron path and the
// manual trigger build it.
//
// Forbid is enforced by string-comparing concurrency_key against active runs
// (CheckForbid) and by a unique index on that same value. A bare job name here
// would be a different string from the one every other producer writes, so a
// reaction-fired run of a Forbid job with no explicit key would neither see a
// cron fire's active run nor collide with it — and two runs of a job whose
// entire purpose is never to overlap would run together, silently.
//
// The default branch is the one that matters: an author who sets Forbid without
// naming a key gets NULL in the column, which is the normal case.
func TestReactionForbidKeyMatchesTheOtherProducers(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, concurrency_key, enabled, synced_at)VALUES ('uid-'||'exclusive', 'exclusive','git','bash','Forbid',NULL,1,'t')`); err != nil {
		t.Fatal(err)
	}

	got, err := s.buildReactionJobParams(ctxb(), "git", "exclusive", "", nil)
	if err != nil {
		t.Fatalf("buildReactionJobParams: %v", err)
	}
	// R2-3/R2-5: cronutil.ConcurrencyKey resolves custom → uid → source/name,
	// and every producer shares it — agreement is the invariant, and the agreed
	// value is now the job's identity.
	if want := "uid-exclusive"; got.ConcurrencyKey != want {
		t.Errorf("concurrency key = %q, want %q — a diverging key would not collide with the "+
			"cron or manual paths' key, defeating Forbid across trigger paths", got.ConcurrencyKey, want)
	}
}

// An explicitly authored key still wins, same as everywhere else.
func TestReactionForbidKeyRespectsAnExplicitKey(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, concurrency_key, enabled, synced_at)VALUES ('uid-'||'shared', 'shared','git','bash','Forbid','db-migration-lock',1,'t')`); err != nil {
		t.Fatal(err)
	}
	got, err := s.buildReactionJobParams(ctxb(), "git", "shared", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConcurrencyKey != "db-migration-lock" {
		t.Errorf("concurrency key = %q, want the authored key", got.ConcurrencyKey)
	}
}

// A reaction that DECIDED to fire but could not must record that as its own
// verdict, not leave the row at its 'pending' claim value.
//
// This is reachable in ordinary operation, not only after a crash: here the
// reacting job is deleted between authoring and firing. Left at 'pending' it
// would be indistinguishable from a crash before the decision — the exact
// ambiguity the claim value exists to prevent — and the event is consumed
// either way, so nothing would ever say what happened.
func TestFailedFireRecordsAnErrorVerdict(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	// The reacting job is REPLACED out from under the reaction: same name, new
	// identity (a purge-and-recreate). A DELETE would cascade the reaction away
	// and leave nothing to fail; since R2-5 the params builder follows the
	// reaction's owner_uid, so a re-SOURCE alone no longer strands it — only a
	// new identity does, and that is exactly what this simulates.
	if _, err := pool.ExecContext(ctxb(),
		`UPDATE jobs SET source='amadeus', uid='uid-remade' WHERE name='down'`); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("pending = %d, want 0 — there is no job to run", n)
	}
	var result, detail string
	if err := pool.QueryRowContext(ctxb(),
		`SELECT result, COALESCE(detail,'') FROM reaction_deliveries WHERE src_run_id='r1'`).
		Scan(&result, &detail); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if result != "error" {
		t.Errorf("delivery result = %q, want \"error\" — a delivery stuck at the 'pending' claim "+
			"value cannot be told apart from a crash before the decision", result)
	}
	if detail == "" {
		t.Error("an error delivery recorded no detail; the row has to say what went wrong")
	}
}

// A disabled reaction reports the switch, not the clock, even when the event is
// also too old: the disabled state is the permanent answer and the one an
// operator can act on.
func TestDisabledReactionReportsTheSwitchNotExpiry(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success", withEnabled(0))
	seedFinishedRun(t, pool, "r-old", "up", "success", 48*time.Hour)

	s.ScanReactions(ctxb())
	s.writeReactionCursor(ctxb(), time.Now().UTC().Add(-72*time.Hour))
	s.ScanReactions(ctxb())

	d := deliveries(t, pool, "down")
	if len(d) != 1 || d[0].Result != "suppressed_disabled" {
		t.Errorf("deliveries = %+v, want suppressed_disabled — reporting 'expired' would send an "+
			"operator to look at timing rather than at the switch they turned off", d)
	}
}

// An ssh-test connectivity probe is not a job execution and must never be an
// upstream, even though it lives in the runs table with a job_name.
func TestSSHTestProbeIsNotAnUpstream(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "SSH Test — host1", "success")
	ts := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by,
		                  trigger_kind, kind, completed_at, created_at)
		VALUES ('r-probe','SSH Test — host1','git','bash','success','t','manual','ssh-test',?,?)`,
		ts, ts); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "down"); n != 0 {
		t.Errorf("an ssh-test probe triggered a reaction (%d pending); a diagnostic must never "+
			"be a trigger", n)
	}
}

// ── RX-11: the other three quadrants ───────────────────────────────────────

// A WORKFLOW target completes the 2×2. The reaction resolves into a pending
// workflow row, and the promotion carries the trigger kind and the reaction
// provenance through the widened firer — the thing that was hardcoded "manual"
// before RX-11, which would have put a lie in History and reset the depth
// ceiling to zero on every hop of a chain.
func TestReactionFiresAWorkflowTarget(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO workflows (name, source, steps, enabled, synced_at)
		VALUES ('cleanup','git','[]',1,'t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome)
		VALUES ('git','workflow','cleanup','after-up','git','job','up','any')`); err != nil {
		t.Fatal(err)
	}
	seedFinishedRun(t, pool, "r1", "up", "failure", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	var kind string
	var depth int
	var originRef, envJSON sql.NullString
	if err := pool.QueryRowContext(ctxb(), `
		SELECT kind, reaction_depth, origin_ref, origin_env_json
		  FROM pending_runs WHERE name = 'cleanup'`).
		Scan(&kind, &depth, &originRef, &envJSON); err != nil {
		t.Fatalf("read pending workflow: %v", err)
	}
	if kind != "workflow" {
		t.Errorf("pending kind = %q, want workflow", kind)
	}
	if depth != 1 || originRef.String != "r1" {
		t.Errorf("provenance = (depth %d, origin %q), want (1, r1)", depth, originRef.String)
	}
	// A workflow pending row has NULL params_json, which is exactly why the
	// stamp and depth had to live in dedicated columns.
	var params sql.NullString
	_ = pool.QueryRowContext(ctxb(),
		`SELECT params_json FROM pending_runs WHERE name='cleanup'`).Scan(&params)
	if params.Valid {
		t.Errorf("workflow pending row carried params_json = %q, want NULL", params.String)
	}
	if !contains(envJSON.String, "AMADEUS_REACTED_TO_OUTCOME") {
		t.Errorf("origin_env_json %q lost the stamp", envJSON.String)
	}

	// Promotion hands the whole envelope to the firer.
	var got PendingWorkflowFire
	fired := false
	s2 := New(pool, quietLog(), nil)
	s2.SetPendingWorkflowFirer(func(_ context.Context, p PendingWorkflowFire) {
		got, fired = p, true
	})
	s2.PromotePending(ctxb())

	if !fired {
		t.Fatal("the promoted workflow reaction never reached the firer")
	}
	if got.TriggerKind != "reaction" {
		t.Errorf("trigger kind = %q, want reaction — hardcoding manual here was the RX-11 bug", got.TriggerKind)
	}
	if got.TriggeredBy != "reactor" {
		t.Errorf("triggered_by = %q, want reactor", got.TriggeredBy)
	}
	if got.ReactionDepth != 1 {
		t.Errorf("depth = %d, want 1 — a depth that resets each hop disarms the ceiling", got.ReactionDepth)
	}
	if got.ReactedToRunID != "r1" {
		t.Errorf("reacted-to = %q, want r1", got.ReactedToRunID)
	}
	if !contains(got.EnvJSON, "AMADEUS_REACTED_TO_NAME") {
		t.Errorf("env %q lost the stamp across promotion", got.EnvJSON)
	}
}

// workflow → workflow, the last quadrant, and the one with no job anywhere in
// it. Nothing in the reactor branches on the pairing; this pins that.
func TestWorkflowToWorkflowReaction(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO workflows (name, source, steps, enabled, synced_at) VALUES
		  ('nightly','git','[]',1,'t'), ('report','git','[]',1,'t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome)
		VALUES ('git','workflow','report','after-nightly','git','workflow','nightly','success')`); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, triggered_by,
		                           trigger_kind, completed_at, created_at)
		VALUES ('wfr','nightly','git','success','t','scheduled',?,?)`, ts, ts); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "report"); n != 1 {
		t.Errorf("workflow→workflow pending = %d, want 1", n)
	}
}

// A pending WORKFLOW run whose definition was renamed away is marked missed
// rather than silently consumed. promoteOne deletes the row before calling the
// firer, so without this check the firer's own lookup failed after the row was
// already gone and the run vanished with only a log line — the gap the job
// branch has always guarded against.
func TestPromotionMarksAMissingWorkflowMissed(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	s.SetPendingWorkflowFirer(func(_ context.Context, _ PendingWorkflowFire) {
		t.Error("the firer must not be called for a workflow that no longer exists")
	})
	if _, err := pool.ExecContext(ctxb(), `
		INSERT INTO pending_runs (id, kind, name, source, run_at, scheduled_by, created_at, status)
		VALUES ('p1','workflow','ghost','git',?,'op@example.com','2026-01-01T00:00:00Z','pending')`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	s.PromotePending(ctxb())

	var status, reason string
	if err := pool.QueryRowContext(ctxb(),
		`SELECT status, COALESCE(miss_reason,'') FROM pending_runs WHERE id='p1'`).
		Scan(&status, &reason); err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if status != "missed" {
		t.Errorf("status = %q, want missed — a vanished workflow must not silently consume the row", status)
	}
	if !contains(reason, "no longer exists") {
		t.Errorf("miss_reason = %q, want it to name the cause", reason)
	}
}

// §2.8 / RX-Q6 — a depth-ceiling hit emits an ACTIVITY row, not just a delivery
// row. A ceiling hit means a cycle static detection missed, which is a defect in
// the graph or in the checker; it has to surface where a human actually looks
// rather than only in a table nobody queries.
func TestDepthCeilingEmitsAnActivityRow(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success")
	seedFinishedRun(t, pool, "r-deep", "up", "success", time.Minute)
	if _, err := pool.ExecContext(ctxb(), `UPDATE runs SET reaction_depth = 4 WHERE id='r-deep'`); err != nil {
		t.Fatal(err)
	}

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	var kind, outcome, actor, category, action, summary string
	var jobName, traceID sql.NullString
	if err := pool.QueryRowContext(ctxb(), `
		SELECT kind, COALESCE(outcome,''), actor, COALESCE(category,''), COALESCE(action,''),
		       COALESCE(summary,''), job_name, trace_id
		  FROM activity WHERE category = 'Reactions'`).
		Scan(&kind, &outcome, &actor, &category, &action, &summary, &jobName, &traceID); err != nil {
		t.Fatalf("no depth-ceiling activity row: %v", err)
	}

	// kind='config', deliberately, and NOT a new 'reaction' kind: auditlog's
	// streamedActivityKinds map drops unknown kinds before they reach audit.log,
	// so a new kind would render in the UI and be invisible to every SIEM while
	// the kind-blind CSV export still contained it.
	if kind != "config" {
		t.Errorf("kind = %q, want config", kind)
	}
	// Not 'failure' — nothing failed, a brake engaged.
	if outcome != "warning" {
		t.Errorf("outcome = %q, want warning", outcome)
	}
	if actor != "reactor" || action != "Depth ceiling" {
		t.Errorf("actor/action = %q/%q, want reactor/Depth ceiling", actor, action)
	}
	// The Activity card titles itself from jobName/workflowName, so without this
	// the row renders as the word "config" and names nothing.
	if jobName.String != "down" {
		t.Errorf("job_name = %q, want the REACTING definition 'down'", jobName.String)
	}
	if traceID.String != "r-deep" {
		t.Errorf("trace_id = %q, want the upstream run that hit the ceiling", traceID.String)
	}
	if !contains(summary, "depth ceiling") {
		t.Errorf("summary %q should say what happened in words", summary)
	}

	// Idempotent: a re-scan of the same event must not emit a second row. The
	// delivery claim's primary key is (reaction, upstream run), so this comes for
	// free — but "for free" is exactly the kind of guarantee that quietly breaks.
	s.ScanReactions(ctxb())
	var n int
	_ = pool.QueryRowContext(ctxb(), `SELECT COUNT(*) FROM activity WHERE category='Reactions'`).Scan(&n)
	if n != 1 {
		t.Errorf("activity rows after a re-scan = %d, want 1", n)
	}
}

// An ORDINARY suppression does not spam the Activity feed. Only the ceiling hit
// is a defect; a calendar or rate suppression is policy working as intended and
// belongs in the delivery log alone.
func TestOrdinarySuppressionEmitsNoActivity(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "up", 1)
	seedJobRow(t, pool, "down", 1)
	seedReaction(t, pool, "down", "r", "up", "success", withEnabled(0))
	seedFinishedRun(t, pool, "r1", "up", "success", time.Minute)

	primeCursor(t, s)
	s.ScanReactions(ctxb())

	var n int
	_ = pool.QueryRowContext(ctxb(), `SELECT COUNT(*) FROM activity WHERE category='Reactions'`).Scan(&n)
	if n != 0 {
		t.Errorf("a disabled-reaction suppression wrote %d activity rows, want 0", n)
	}
}
