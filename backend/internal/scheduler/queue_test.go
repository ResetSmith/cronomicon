package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

// QP — the Queue concurrency policy.
//
// Queue's whole claim is that a fire which would have been LOST is instead
// parked and run later. So the tests are about the two ways that claim fails:
// the fire being lost anyway (no row, no record), and the queue growing without
// bound behind a job that never finishes.

func seedQueueJob(t *testing.T, pool *sql.DB, name, policy string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, enabled, synced_at)VALUES ('uid-'||?, ?, 'git', 'bash', 'prod', ?, 1, 't')`, name, name, policy); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

func seedActiveKeyedRun(t *testing.T, pool *sql.DB, id, job, key string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO runs (id, job_name, job_source, run_type, status, concurrency_key,
		                   triggered_by, trigger_kind, created_at)
		 VALUES (?, ?, 'git', 'bash', 'running', ?, 'scheduler', 'scheduled', '2026-08-11T00:00:00Z')`,
		id, job, key); err != nil {
		t.Fatalf("seed active run: %v", err)
	}
}

func queueDepth(t *testing.T, pool *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM pending_runs WHERE concurrency_key = ? AND gate_kind = 'concurrency'`, key).Scan(&n); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	return n
}

// A Queue-policy fire meeting a held gate parks instead of being lost.
func TestQueuePolicyParksTheFire(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")

	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")

	if n := countRuns(t, pool, "patch", "queued"); n != 0 {
		t.Errorf("the fire enqueued %d runs, want 0 — the gate was held", n)
	}
	if n := countRuns(t, pool, "patch", "skipped"); n != 0 {
		t.Errorf("the fire was recorded as %d skips — Queue parks, it does not lose the fire", n)
	}
	if d := queueDepth(t, pool, "git/patch"); d != 1 {
		t.Fatalf("queue depth = %d, want 1", d)
	}

	// The parked row must carry the policy, or promotion cannot re-judge the
	// gate and would fire it immediately.
	var params string
	_ = pool.QueryRow(`SELECT params_json FROM pending_runs WHERE gate_kind='concurrency'`).Scan(&params)
	if params == "" {
		t.Fatal("the queued row has no frozen params")
	}
	if !strings.Contains(params, cronutil.PolicyQueue) {
		t.Errorf("the queued row's params do not carry the policy: %s", params)
	}
}

// Forbid is unchanged: it still loses the fire and records a skip.
func TestForbidStillSkipsRatherThanQueues(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyForbid)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")

	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyForbid, "git/patch", "nightly", "")

	if n := countRuns(t, pool, "patch", "skipped"); n != 1 {
		t.Errorf("Forbid recorded %d skips, want 1", n)
	}
	if d := queueDepth(t, pool, "git/patch"); d != 0 {
		t.Errorf("Forbid parked %d runs; it must not queue", d)
	}
}

// The cap is the runaway brake. Beyond it the fire falls back to Forbid's
// behaviour and — crucially — SAYS so, rather than vanishing.
func TestQueueCapFallsBackToSkipAndRecordsIt(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")

	for range QueueCap + 2 {
		s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")
	}

	if d := queueDepth(t, pool, "git/patch"); d != QueueCap {
		t.Errorf("queue depth = %d, want the cap of %d — an unbounded queue behind a wedged job is the runaway this prevents", d, QueueCap)
	}
	rows := skippedRows(t, pool, "patch")
	if len(rows) == 0 {
		t.Fatal("a refused fire left no record at all — invisible is worse than skipped")
	}
	if rows[0] != QueueFullReason {
		t.Errorf("queue-full reason = %q, want %q", rows[0], QueueFullReason)
	}
}

// A refusal must record even when another job shares the concurrency key.
//
// The cap fallback used to record under dedupeEpisode, which asks "is the most
// recent run for THIS KEY already skipped?" — a question that cannot tell a
// queue-full refusal from a Forbid skip. A concurrency_key is a job's own name by
// default, but an operator may set a CUSTOM one to serialise several jobs against
// a shared resource, and then the two suppressions land in the same bucket and
// whichever arrives second collapses into the first and vanishes. That is the
// CAL-8 leak one level down, and the fix is the one the pause and cap paths
// already use: dedupeEpisodeReason, and no key on the row so the Forbid rule
// cannot see it either.
//
// Both orders are driven, because independence has to hold in both directions.
func TestQueueFullRefusalRecordsBesideAForbidSkipOnASharedKey(t *testing.T) {
	// One custom key, two jobs — the arrangement that used to refuse in silence.
	const key = "nightly-patching"

	for _, tc := range []struct {
		name        string
		forbidFirst bool
	}{
		{"Forbid skip lands first", true},
		{"queue-full refusal lands first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)

			seedQueueJob(t, pool, "guard", cronutil.PolicyForbid)
			seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
			seedActiveKeyedRun(t, pool, "active-1", "guard", key)

			// Fill the queue to the cap, so the NEXT Queue fire is refused.
			for range QueueCap {
				s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, key, "nightly", "")
			}
			if d := queueDepth(t, pool, key); d != QueueCap {
				t.Fatalf("setup: queue depth = %d, want the cap of %d", d, QueueCap)
			}

			forbidFire := func() { s.fire("git", "guard", "", "bash", "prod", cronutil.PolicyForbid, key, "nightly", "") }
			refusedFire := func() { s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, key, "nightly", "") }
			if tc.forbidFirst {
				forbidFire()
				refusedFire()
			} else {
				refusedFire()
				forbidFire()
			}

			refused := skippedRows(t, pool, "patch")
			if len(refused) != 1 {
				t.Fatalf("the queue-full refusal left %d rows, want 1 — a fire that is neither run nor queued "+
					"nor recorded is invisible, and a job sharing the key must not be able to silence it: %v", len(refused), refused)
			}
			if refused[0] != QueueFullReason {
				t.Errorf("queue-full reason = %q, want %q", refused[0], QueueFullReason)
			}
			forbidden := skippedRows(t, pool, "guard")
			if len(forbidden) != 1 {
				t.Fatalf("the Forbid skip left %d rows, want 1 — and the refusal must not have swallowed it either: %v", len(forbidden), forbidden)
			}
			if forbidden[0] == QueueFullReason || forbidden[0] == "" {
				t.Errorf("Forbid reason = %q, want its own text — the two suppressions must stay distinguishable in History", forbidden[0])
			}

			// The refusal carries NO key, which is what keeps it invisible to the
			// key-based Forbid de-dupe; the Forbid skip keeps one, which is what that
			// rule reads.
			var refusedKey sql.NullString
			if err := pool.QueryRow(
				`SELECT concurrency_key FROM runs WHERE job_name='patch' AND status='skipped'`).Scan(&refusedKey); err != nil {
				t.Fatalf("read refusal row: %v", err)
			}
			if refusedKey.Valid {
				t.Errorf("the queue-full row carries concurrency_key %q, want NULL — with a key it is back inside the "+
					"Forbid de-dupe's bucket, which is the collapse this pins", refusedKey.String)
			}
			var forbidKey sql.NullString
			if err := pool.QueryRow(
				`SELECT concurrency_key FROM runs WHERE job_name='guard' AND status='skipped'`).Scan(&forbidKey); err != nil {
				t.Fatalf("read Forbid row: %v", err)
			}
			if forbidKey.String != key {
				t.Errorf("the Forbid skip's concurrency_key = %q, want %q", forbidKey.String, key)
			}
		})
	}
}

// The queued run promotes when the gate clears — and not before.
func TestQueuedRunPromotesOnlyWhenTheGateClears(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")
	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")
	if d := queueDepth(t, pool, "git/patch"); d != 1 {
		t.Fatalf("setup: queue depth = %d, want 1", d)
	}

	// Gate still held → the promoter must leave it alone.
	s.PromotePending(ctx)
	if d := queueDepth(t, pool, "git/patch"); d != 1 {
		t.Errorf("the promoter fired a queued run while the gate was still held (depth %d)", d)
	}
	if n := countRuns(t, pool, "patch", "queued"); n != 0 {
		t.Errorf("a run was enqueued while the gate was held (%d)", n)
	}

	// Gate clears.
	if _, err := pool.Exec(`UPDATE runs SET status='success' WHERE id='active-1'`); err != nil {
		t.Fatalf("clear gate: %v", err)
	}
	s.PromotePending(ctx)

	if d := queueDepth(t, pool, "git/patch"); d != 0 {
		t.Errorf("the queued row survived promotion (depth %d)", d)
	}
	if n := countRuns(t, pool, "patch", "queued"); n != 1 {
		t.Errorf("enqueued %d runs after the gate cleared, want 1", n)
	}
	// The promoted run carries the key, or the NEXT fire has nothing to queue
	// behind and the policy silently degrades to Allow.
	var key sql.NullString
	_ = pool.QueryRow(`SELECT concurrency_key FROM runs WHERE job_name='patch' AND status='queued'`).Scan(&key)
	if key.String != "git/patch" {
		t.Errorf("the promoted run's concurrency_key = %q, want git/patch", key.String)
	}
}

// A Queue-policy run must hold the gate, exactly as a Forbid one does — that is
// what the partial unique index enforces, and what makes "one at a time" true.
func TestQueuePolicyRunsCarryTheKey(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")

	var key sql.NullString
	if err := pool.QueryRow(`SELECT concurrency_key FROM runs WHERE job_name='patch'`).Scan(&key); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if key.String != "git/patch" {
		t.Errorf("a Queue run's concurrency_key = %q, want git/patch — without it nothing queues behind it", key.String)
	}
}

// Allow is untouched: no key, no queue, overlap freely.
func TestAllowPolicyNeitherKeysNorQueues(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyAllow)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")
	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyAllow, "git/patch", "nightly", "")

	if n := countRuns(t, pool, "patch", "queued"); n != 1 {
		t.Errorf("Allow enqueued %d runs, want 1 — it overlaps by definition", n)
	}
	if d := queueDepth(t, pool, "git/patch"); d != 0 {
		t.Errorf("Allow parked %d runs", d)
	}
	var key sql.NullString
	_ = pool.QueryRow(`SELECT concurrency_key FROM runs WHERE job_name='patch' AND status='queued'`).Scan(&key)
	if key.Valid {
		t.Errorf("an Allow run carries concurrency_key %q; PP-L8 says only gate-holding policies do", key.String)
	}
}

// A run queued behind a permanently wedged job expires rather than waiting
// forever — AR's 24-hour catch-up rule, inherited for free (PF-Q10).
func TestQueuedRunExpiresOnTheCatchUpGrace(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)

	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")
	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")

	// Backdate the parked row past the grace window.
	if _, err := pool.Exec(
		`UPDATE pending_runs SET run_at = '2020-01-01T00:00:00Z' WHERE gate_kind='concurrency'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	s.PromotePending(ctx)

	var status, reason string
	if err := pool.QueryRow(
		`SELECT status, COALESCE(miss_reason,'') FROM pending_runs WHERE gate_kind='concurrency'`).Scan(&status, &reason); err != nil {
		t.Fatalf("read parked row: %v", err)
	}
	if status != "missed" {
		t.Errorf("a run queued for years is %q, want missed — it must not wait forever", status)
	}
	if reason == "" {
		t.Error("the expired row records no reason")
	}
}

// ─── FX-B1: the PRODUCER half — promotion must carry the fire instant ─────────

// The detector-side test seeds a runs row with scheduled_for already set, which
// proves the detector reads it but not that anything ever writes it. This drives
// the real promotion path: without it, deleting the stamp in promoteOne leaves
// the whole suite green while the false-page regression returns in full.
func TestPromotionCarriesTheFireInstantOntoTheRun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()

	fireAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")

	// A fire meets the held gate and parks.
	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")
	if _, err := pool.Exec(`UPDATE pending_runs SET created_at = ?, run_at = ? WHERE gate_kind='concurrency'`,
		fireAt.Format(time.RFC3339), fireAt.Format(time.RFC3339)); err != nil {
		t.Fatalf("backdate parked row: %v", err)
	}
	// The gate clears, and the row promotes.
	if _, err := pool.Exec(`UPDATE runs SET status='success', completed_at='2026-01-01T00:00:00Z' WHERE id='active-1'`); err != nil {
		t.Fatalf("clear gate: %v", err)
	}
	s.PromotePending(ctx)

	var scheduledFor sql.NullString
	if err := pool.QueryRow(
		`SELECT scheduled_for FROM runs WHERE job_name='patch' AND status='queued'`).Scan(&scheduledFor); err != nil {
		t.Fatalf("promoted run not found: %v", err)
	}
	if !scheduledFor.Valid {
		t.Fatal("the promoted run carries no scheduled_for — the pending row that held the fire " +
			"instant has just been deleted, so nothing anywhere now knows which fire this run was, " +
			"and the missed-run detector will page for it")
	}
	got, err := time.Parse(time.RFC3339, scheduledFor.String)
	if err != nil {
		t.Fatalf("unparseable scheduled_for %q: %v", scheduledFor.String, err)
	}
	if !got.Equal(fireAt) {
		t.Errorf("scheduled_for = %s, want the fire instant %s", got.Format(time.RFC3339), fireAt.Format(time.RFC3339))
	}
}

// An ad-hoc deferral has no schedule expecting it, so it records nothing —
// otherwise a manual run would start explaining cron fires it has nothing to do
// with.
func TestAdHocPromotionCarriesNoFireInstant(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()

	seedQueueJob(t, pool, "patch", cronutil.PolicyAllow)
	if _, err := InsertPendingRun(ctx, pool, "job", "patch", "git", "",
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "op@example.com",
		&EnqueueParams{JobName: "patch", JobSource: "git", RunType: "bash",
			TriggerKind: "manual", TriggeredBy: "op@example.com"}); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	s.PromotePending(ctx)

	var scheduledFor sql.NullString
	if err := pool.QueryRow(`SELECT scheduled_for FROM runs WHERE job_name='patch'`).Scan(&scheduledFor); err != nil {
		t.Fatalf("promoted run not found: %v", err)
	}
	if scheduledFor.Valid {
		t.Errorf("an ad-hoc deferral recorded scheduled_for = %q — no schedule expected it, so it "+
			"must not be able to explain away a cron fire", scheduledFor.String)
	}
}

// FX-A3 × FX-B1 — the recycle-bin hold must not destroy the QUEUE gate.
//
// gate_kind='concurrency' is not a label, it is the record that a row IS a
// queued fire, and four readers depend on it: the missed-run detector's parked
// check, the scheduled_for stamp above, the queue-depth count behind QueueCap,
// and the upcoming projection. The first version of the hold overwrote it and
// then cleared it to NULL on restore, breaking all four for the row's lifetime.
func TestRecycleBinHoldDoesNotClobberTheQueueGate(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()

	seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
	seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")
	s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")
	if d := queueDepth(t, pool, "git/patch"); d != 1 {
		t.Fatalf("setup: queue depth = %d, want 1", d)
	}

	// Bin the job, tick, restore, tick.
	if _, err := pool.Exec(`UPDATE jobs SET deleted_at='2026-08-12T00:00:00Z' WHERE name='patch'`); err != nil {
		t.Fatalf("bin job: %v", err)
	}
	s.PromotePending(ctx)
	var gate string
	_ = pool.QueryRow(`SELECT COALESCE(gate_kind,'') FROM pending_runs`).Scan(&gate)
	if gate != "concurrency" {
		t.Fatalf("gate_kind = %q after binning a QUEUED row, want it left as concurrency — "+
			"overwriting it makes the fire read as a silent miss and un-counts it from QueueCap", gate)
	}

	if _, err := pool.Exec(`UPDATE jobs SET deleted_at=NULL WHERE name='patch'`); err != nil {
		t.Fatalf("restore job: %v", err)
	}
	s.PromotePending(ctx)
	_ = pool.QueryRow(`SELECT COALESCE(gate_kind,'') FROM pending_runs`).Scan(&gate)
	if gate != "concurrency" {
		t.Errorf("gate_kind = %q after restore, want concurrency — clearing it to NULL loses the "+
			"queue gate permanently", gate)
	}
	if d := queueDepth(t, pool, "git/patch"); d != 1 {
		t.Errorf("queue depth = %d after a bin/restore round trip, want 1 — QueueCap now undercounts", d)
	}
}
