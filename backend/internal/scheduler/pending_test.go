package scheduler_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// AR — the pending-run lifecycle: park → promote → run row; the missed cutoff;
// the cancel race; the workflow firer path.

func seedPendingJob(t *testing.T, pool *sql.DB) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, synced_at)VALUES ('uid-'||'deferred-job', 'deferred-job', 'git', 'bash', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

func insertPending(t *testing.T, pool *sql.DB, runAt time.Time) string {
	t.Helper()
	id, err := scheduler.InsertPendingRun(context.Background(), pool, "job", "deferred-job", "git", "",
		runAt.UTC().Format(time.RFC3339), "op@example.com",
		&scheduler.EnqueueParams{
			JobName: "deferred-job", JobSource: "git", RunType: "bash",
			TriggerKind: "manual", TriggeredBy: "op@example.com",
			OverrideJSON: `{"scheduledFor":"` + runAt.UTC().Format(time.RFC3339) + `","scheduledBy":"op@example.com"}`,
		})
	if err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	return id
}

// A due pending run promotes into an ordinary queued runs row, carrying its
// frozen envelope, and the pending row is consumed.
func TestPendingRunPromotes(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-time.Minute)) // due
	s.PromotePending(ctx)

	var status, triggeredBy, override string
	if err := pool.QueryRowContext(ctx, `
		SELECT status, triggered_by, COALESCE(override_json,'') FROM runs WHERE job_name = 'deferred-job'`).
		Scan(&status, &triggeredBy, &override); err != nil {
		t.Fatalf("promoted run not found: %v", err)
	}
	if status != "queued" {
		t.Errorf("promoted run status = %q, want queued", status)
	}
	if triggeredBy != "op@example.com" {
		t.Errorf("triggered_by = %q — the scheduling operator must survive promotion", triggeredBy)
	}
	if override == "" || !contains(override, "scheduledFor") {
		t.Errorf("override envelope lost in promotion: %q", override)
	}

	var left int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs`).Scan(&left)
	if left != 0 {
		t.Errorf("pending row not consumed: %d left", left)
	}
}

// A run not yet due is untouched.
func TestPendingRunNotDueYet(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(time.Hour))
	s.PromotePending(ctx)

	var runs, pending int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs)
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs WHERE status='pending'`).Scan(&pending)
	if runs != 0 || pending != 1 {
		t.Errorf("future run touched: %d runs, %d pending", runs, pending)
	}
}

// Past the catch-up grace, a row is marked missed — kept visible, never fired.
// (This IS the restart-catch-up rule: ≤24h late fires, later is missed.)
func TestPendingRunMissedPastGrace(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-25*time.Hour))
	s.PromotePending(ctx)

	// Nothing EXECUTED. The 'skipped' marker FX-B2 writes is not a fire — it is
	// the record that a fire did not happen — so it is excluded here and asserted
	// on its own below.
	// status='queued' specifically: that is what a real promotion inserts, and
	// counting "anything not skipped" would also pass if a second, spurious
	// marker appeared. The FX-B2 marker is asserted in its own test below.
	var runs int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE status = 'queued'`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("a run fired %d times for an instant 25h gone", runs)
	}
	var status, reason string
	if err := pool.QueryRowContext(ctx,
		`SELECT status, COALESCE(miss_reason,'') FROM pending_runs`).Scan(&status, &reason); err != nil {
		t.Fatalf("missed row vanished: %v", err)
	}
	if status != "missed" || reason == "" {
		t.Errorf("status=%q reason=%q — want a kept, explained missed row", status, reason)
	}
}

// Within the grace, a late run (server was down at its instant) still fires.
func TestPendingRunCatchUpWithinGrace(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-2*time.Hour))
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE status='queued'`).Scan(&runs)
	if runs != 1 {
		t.Errorf("2h-late run should catch up and fire, got %d runs", runs)
	}
}

// A deleted (cancelled) row is simply gone — the promote/cancel race resolves
// to whoever deletes first, and a cancelled run never fires.
func TestPendingRunCancelWins(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	id := insertPending(t, pool, time.Now().Add(-time.Minute))
	if _, err := pool.ExecContext(ctx, `DELETE FROM pending_runs WHERE id = ?`, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("cancelled run fired anyway: %d runs", runs)
	}
}

// Deleting the job cascades its parked runs (migration 790 trigger).
func TestPendingRunCascadesOnJobDelete(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(time.Hour))
	if _, err := pool.ExecContext(ctx, `DELETE FROM jobs WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	var left int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs`).Scan(&left)
	if left != 0 {
		t.Errorf("pending rows survived their job's deletion: %d", left)
	}
}

// A pending WORKFLOW run fires through the injected firer, with the scheduling
// operator preserved as the actor.
func TestPendingWorkflowRunFires(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO workflows (name, source, steps, synced_at) VALUES ('deferred-wf', 'git', '[]', 't')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	s := scheduler.New(pool, discardLog(), nil)

	fired := make(chan scheduler.PendingWorkflowFire, 1)
	s.SetPendingWorkflowFirer(func(_ context.Context, p scheduler.PendingWorkflowFire) {
		fired <- p
	})
	if _, err := scheduler.InsertPendingRun(ctx, pool, "workflow", "deferred-wf", "git", "",
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "op@example.com", nil); err != nil {
		t.Fatalf("insert pending workflow: %v", err)
	}
	s.PromotePending(ctx)

	select {
	case got := <-fired:
		if got.Source != "git" || got.Name != "deferred-wf" || got.TriggeredBy != "op@example.com" {
			t.Errorf("firer got %+v", got)
		}
		// An ad-hoc deferral is still manual, and carries no reaction
		// provenance — the widened envelope must not relabel the AR path.
		if got.TriggerKind != "manual" {
			t.Errorf("trigger kind = %q, want manual for an ad-hoc deferral", got.TriggerKind)
		}
		if got.ReactionDepth != 0 || got.ReactedToRunID != "" || got.EnvJSON != "" {
			t.Errorf("ad-hoc deferral carried reaction provenance: %+v", got)
		}
	default:
		t.Fatal("pending workflow run did not fire")
	}
	var left int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs`).Scan(&left)
	if left != 0 {
		t.Errorf("pending workflow row not consumed: %d left", left)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// ─── FX-A3: the recycle bin holds a parked run, it does not destroy it ────────

// Binning is reversible by design; marking a parked run `missed` is not, and no
// restore path revives one. So a bin-and-restore inside the catch-up window used
// to silently destroy a run the operator never cancelled — and explain it with
// "job no longer exists", which was untrue of a job sitting in the bin. A binned
// owner must HOLD the row instead: promotion retries every tick.
func TestPendingRunHeldWhileOwnerIsBinned(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-time.Minute)) // due
	if _, err := pool.ExecContext(ctx,
		`UPDATE jobs SET deleted_at = '2026-08-12T00:00:00Z' WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("bin job: %v", err)
	}
	s.PromotePending(ctx)

	var runs int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("a binned job's parked run fired %d time(s) — the bin must stop it", runs)
	}
	var status, reason string
	if err := pool.QueryRowContext(ctx,
		`SELECT status, COALESCE(miss_reason,'') FROM pending_runs`).Scan(&status, &reason); err != nil {
		t.Fatalf("pending row vanished: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q (reason %q), want pending — binning a job destroyed a parked run "+
			"that restoring the job can no longer bring back", status, reason)
	}

	// Restore, and the held row fires: the whole point of holding it.
	if _, err := pool.ExecContext(ctx,
		`UPDATE jobs SET deleted_at = NULL WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("restore job: %v", err)
	}
	s.PromotePending(ctx)

	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE status='queued'`).Scan(&runs)
	if runs != 1 {
		t.Errorf("queued runs after restore = %d, want 1 — the held row did not resume", runs)
	}
}

// A job that is genuinely GONE still misses loudly. The bin case above must not
// have turned this into a silent hold that waits forever on a definition that
// will never come back.
//
// The gone-ness is staged as a RENAME, not a delete: a hard delete cascades the
// pending row away entirely (TestPendingRunCascadesOnJobDelete), so a rename is
// the only way the row outlives its owner — which is exactly what the branch's
// own comment says it exists for.
func TestPendingRunStillMissesWhenOwnerIsGone(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-time.Minute))
	if _, err := pool.ExecContext(ctx,
		`UPDATE jobs SET name = 'renamed-job' WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("rename job: %v", err)
	}
	s.PromotePending(ctx)

	var status, reason string
	if err := pool.QueryRowContext(ctx,
		`SELECT status, COALESCE(miss_reason,'') FROM pending_runs`).Scan(&status, &reason); err != nil {
		t.Fatalf("pending row vanished: %v", err)
	}
	if status != "missed" {
		t.Errorf("status = %q, want missed — a purged owner is not coming back", status)
	}
	if !strings.Contains(reason, "no longer exists") {
		t.Errorf("miss_reason = %q, want it to say the job no longer exists", reason)
	}
}

// A held row must be VISIBLE, not merely alive. /schedules/upcoming classifies a
// parked row as missed, gate-queued, or future-dated; a held row is none of those
// (its run_at is already past), so without a gate stamp it appeared in neither
// list — invisible in the product for up to 24h, and uncancellable, since Cancel
// needs the pendingId only that endpoint carries. Same trap the concurrency
// branch was written to escape.
func TestHeldPendingRunIsMarkedWithItsGate(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-time.Minute))
	if _, err := pool.ExecContext(ctx,
		`UPDATE jobs SET deleted_at = '2026-08-12T00:00:00Z' WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("bin job: %v", err)
	}
	s.PromotePending(ctx)

	var gate string
	if err := pool.QueryRowContext(ctx,
		`SELECT COALESCE(gate_kind,'') FROM pending_runs`).Scan(&gate); err != nil {
		t.Fatalf("read gate: %v", err)
	}
	if gate != scheduler.GateRecycleBin {
		t.Fatalf("gate_kind = %q, want %q — an unstamped held row shows up in neither the "+
			"upcoming nor the missed list", gate, scheduler.GateRecycleBin)
	}

	// Restoring the owner releases the hold rather than leaving a stale gate.
	if _, err := pool.ExecContext(ctx, `UPDATE jobs SET deleted_at = NULL WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("restore job: %v", err)
	}
	// Park it again so promotion has something to release and then fire.
	s.PromotePending(ctx)
	var left int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs WHERE gate_kind = ?`, scheduler.GateRecycleBin).Scan(&left)
	if left != 0 {
		t.Errorf("%d row(s) still carry the recycle-bin gate after the owner was restored", left)
	}
}

// The workflow branch holds too — it was the untested half.
func TestPendingWorkflowRunHeldWhileOwnerIsBinned(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO workflows (name, source, steps, synced_at)
		VALUES ('deferred-wf', 'git', '[]', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	var fired int
	s := scheduler.New(pool, discardLog(), nil)
	s.SetPendingWorkflowFirer(func(context.Context, scheduler.PendingWorkflowFire) { fired++ })

	if _, err := scheduler.InsertPendingRun(ctx, pool, "workflow", "deferred-wf", "git", "",
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "op@example.com", nil); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`UPDATE workflows SET deleted_at = '2026-08-12T00:00:00Z' WHERE name = 'deferred-wf'`); err != nil {
		t.Fatalf("bin workflow: %v", err)
	}
	s.PromotePending(ctx)

	if fired != 0 {
		t.Fatalf("a binned workflow's parked run fired %d time(s)", fired)
	}
	var status, gate string
	if err := pool.QueryRowContext(ctx,
		`SELECT status, COALESCE(gate_kind,'') FROM pending_runs`).Scan(&status, &gate); err != nil {
		t.Fatalf("pending row vanished: %v", err)
	}
	if status != "pending" || gate != scheduler.GateRecycleBin {
		t.Errorf("status=%q gate=%q, want pending/%s", status, gate, scheduler.GateRecycleBin)
	}
}

// When a held row finally expires, the reason must name the CAUSE. "Past the
// grace" alone sends the operator looking for an outage that never happened.
func TestHeldPendingRunExpiryNamesTheRecycleBin(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	id := insertPending(t, pool, time.Now().Add(-time.Minute))
	if _, err := pool.ExecContext(ctx,
		`UPDATE jobs SET deleted_at = '2026-08-12T00:00:00Z' WHERE name = 'deferred-job'`); err != nil {
		t.Fatalf("bin job: %v", err)
	}
	s.PromotePending(ctx) // stamps the hold

	// Age the row past the catch-up grace and promote again.
	if _, err := pool.ExecContext(ctx, `UPDATE pending_runs SET run_at = ? WHERE id = ?`,
		time.Now().Add(-30*time.Hour).UTC().Format(time.RFC3339), id); err != nil {
		t.Fatalf("age row: %v", err)
	}
	s.PromotePending(ctx)

	var status, reason string
	if err := pool.QueryRowContext(ctx,
		`SELECT status, COALESCE(miss_reason,'') FROM pending_runs`).Scan(&status, &reason); err != nil {
		t.Fatalf("row vanished: %v", err)
	}
	if status != "missed" {
		t.Fatalf("status = %q, want missed once past the grace", status)
	}
	if !strings.Contains(reason, "recycle bin") {
		t.Errorf("miss_reason = %q, want it to name the recycle bin as the cause", reason)
	}
}

// ─── FX-B2: an expired parked run leaves evidence ────────────────────────────

// Until FX-B2 this path wrote status='missed' and a log line and nothing else:
// no runs row, so History and analytics showed nothing; no notification, so
// nobody was told. A scheduled fire that never happened produced zero signal
// anywhere an operator looks.
func TestExpiredPendingRunLeavesADurableMissedRow(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	insertPending(t, pool, time.Now().Add(-25*time.Hour))
	s.PromotePending(ctx)

	var status, reason, triggeredBy string
	if err := pool.QueryRowContext(ctx, `
		SELECT status, COALESCE(queued_reason,''), COALESCE(triggered_by,'')
		  FROM runs WHERE job_name = 'deferred-job'`).Scan(&status, &reason, &triggeredBy); err != nil {
		t.Fatalf("no runs row for an expired parked run (%v) — it is invisible in History, "+
			"analytics and notifications alike", err)
	}
	if status != "skipped" {
		t.Errorf("marker status = %q, want skipped", status)
	}
	// The exact constant, not a substring: it is a dedupeEpisodeReason key, so a
	// drifting or decorated variant silently stops collapsing with the detector's
	// own markers and produces a second row per episode.
	if reason != "Missed: the schedule expected a fire and no run appeared" {
		t.Errorf("queued_reason = %q, want the bare reasonMissedFire constant — a variable "+
			"tail would make every miss its own de-dupe episode", reason)
	}
	if triggeredBy != "scheduler" {
		t.Errorf("triggered_by = %q, want scheduler — nobody clicked this", triggeredBy)
	}
}

// The marker is stamped at the instant the run was MEANT to happen, so History
// places it where the operator will look for it rather than at sweep time.
func TestExpiredPendingRunMarkerIsStampedAtTheIntendedInstant(t *testing.T) {
	pool := openPool(t)
	seedPendingJob(t, pool)
	s := scheduler.New(pool, discardLog(), nil)
	ctx := context.Background()

	intended := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	insertPending(t, pool, intended)
	s.PromotePending(ctx)

	var createdAt string
	if err := pool.QueryRowContext(ctx,
		`SELECT created_at FROM runs WHERE job_name = 'deferred-job'`).Scan(&createdAt); err != nil {
		t.Fatalf("no marker row: %v", err)
	}
	got, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("unparseable created_at %q: %v", createdAt, err)
	}
	if d := got.Sub(intended); d > time.Minute || d < -time.Minute {
		t.Errorf("marker created_at = %s, want ~%s (the intended instant), off by %s",
			got.Format(time.RFC3339), intended.Format(time.RFC3339), d)
	}
}
