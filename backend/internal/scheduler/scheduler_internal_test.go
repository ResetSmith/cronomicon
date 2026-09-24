package scheduler

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// In-package tests reach the unexported reload/fire internals and the cron
// engine's registered entries, which the external scheduler_test package can't.

func mustPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func seedJobRow(t *testing.T, pool *sql.DB, name string, enabled int) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (uid, name, run_type, concurrency_policy, enabled, synced_at)
		 VALUES ('uid-'||?, ?, 'bash', 'Allow', ?, 't')`, name, name, enabled); err != nil {
		t.Fatalf("seed job %q: %v", name, err)
	}
}

func seedSchedule(t *testing.T, pool *sql.DB, kind, owner, name, cron string, pos int) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position) VALUES (?, ?, ?, ?, ?)`,
		kind, owner, name, cron, pos); err != nil {
		t.Fatalf("seed schedule %s/%s: %v", owner, name, err)
	}
}

// TestReloadRegistersEntriesAndSkipsDisabled checks that Reload registers one
// cron entry per (definition, schedule) across jobs and workflows, and skips
// disabled definitions.
func TestReloadRegistersEntriesAndSkipsDisabled(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	seedJobRow(t, pool, "multi", 1)
	seedSchedule(t, pool, "job", "multi", "default", "0 2 * * *", 0)
	seedSchedule(t, pool, "job", "multi", "verify", "0 */4 * * *", 1)

	seedJobRow(t, pool, "off", 0) // disabled — its schedule must NOT register
	seedSchedule(t, pool, "job", "off", "default", "0 5 * * *", 0)

	if _, err := pool.ExecContext(ctx, `INSERT INTO workflows (name, enabled, synced_at) VALUES ('wf', 1, 't')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	seedSchedule(t, pool, "workflow", "wf", "default", "0 1 * * *", 0)

	s := New(pool, quietLog(), nil)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// 2 (multi job) + 0 (disabled job) + 1 (workflow) = 3.
	if n := len(s.cr.Entries()); n != 3 {
		t.Fatalf("registered %d cron entries, want 3 (2 job + 1 workflow; disabled excluded)", n)
	}
}

// TestFireHonorsPauseAndPersistsTraceability checks that fire() skips a paused
// job and otherwise enqueues a run carrying the schedule name + env snapshot.
func TestFireHonorsPauseAndPersistsTraceability(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at) VALUES ('git', 'job', 'paused', 'op', 't')`); err != nil {
		t.Fatalf("pause: %v", err)
	}

	s := New(pool, quietLog(), nil)

	s.fire("git", "paused", "", "bash", "", "Allow", "", "default", "")
	var n int
	_ = pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE job_name='paused' AND status IN ('queued','running')`).Scan(&n)
	if n != 0 {
		t.Errorf("paused job enqueued %d runs, want 0", n)
	}
	// SL-A: the fire still leaves a terminal 'skipped' audit row, so a paused
	// entry is distinguishable from a scheduler that never fired at all. This
	// assertion used to be COUNT(*) = 0 over every status; the suppression
	// becoming visible is the change, not a regression.
	var skipped int
	_ = pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE job_name='paused' AND status='skipped'`).Scan(&skipped)
	if skipped != 1 {
		t.Errorf("paused job recorded %d skipped runs, want 1", skipped)
	}

	s.fire("git", "free", "", "bash", "prod", "Allow", "", "nightly", `{"K":"v"}`)
	var sched, env sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT schedule_name, env_json FROM runs WHERE job_name='free'`).Scan(&sched, &env); err != nil {
		t.Fatalf("fetch free run: %v", err)
	}
	if sched.String != "nightly" || env.String != `{"K":"v"}` {
		t.Errorf("schedule_name/env_json = %q/%q, want nightly/{\"K\":\"v\"}", sched.String, env.String)
	}
}

// TestFireMergesJobLevelEnv covers JC11 on the scheduled path: a job's job-level
// env (jobs.env_json) is the BASE layer, the firing schedule's env wins on key
// collision (Q-JC9), and a job-only key survives into the run's env_json. A job
// with no job-level env is an exact no-op (R2).
func TestFireMergesJobLevelEnv(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, env_json, synced_at)VALUES ('uid-'||'jenv', 'jenv', 'git', 'bash', 'Allow', 1, '{"X":"job","Y":"job"}', 't')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	s := New(pool, quietLog(), nil)

	// Schedule env {X:sched} wins X; the job-only key Y survives.
	s.fire("git", "jenv", "", "bash", "prod", "Allow", "", "nightly", `{"X":"sched"}`)
	var env sql.NullString
	if err := pool.QueryRowContext(ctx, `SELECT env_json FROM runs WHERE job_name='jenv'`).Scan(&env); err != nil {
		t.Fatalf("fetch jenv run: %v", err)
	}
	if env.String != `{"X":"sched","Y":"job"}` {
		t.Errorf("scheduled env_json = %q, want {\"X\":\"sched\",\"Y\":\"job\"}", env.String)
	}

	// A job with no job-level env is a value-preserving no-op (R2). The merge
	// re-marshals the schedule layer, normalizing key order — which is identical to
	// how every env writer already stores it (always marshaled from a map ⇒ sorted
	// keys), so run snapshots are byte-for-byte unchanged. Seed a deliberately
	// out-of-sorted-order multi-key schedule env to pin that the no-op preserves
	// VALUE and yields the sorted form, not source byte order.
	seedJobRow(t, pool, "plain", 1)
	s.fire("git", "plain", "", "bash", "", "Allow", "", "default", `{"ZED":"1","ABC":"2"}`)
	if err := pool.QueryRowContext(ctx, `SELECT env_json FROM runs WHERE job_name='plain'`).Scan(&env); err != nil {
		t.Fatalf("fetch plain run: %v", err)
	}
	if env.String != `{"ABC":"2","ZED":"1"}` {
		t.Errorf("no-job-env run env_json = %q, want sorted {\"ABC\":\"2\",\"ZED\":\"1\"} (value-preserving no-op)", env.String)
	}
}

// TestFireWorkflowHonorsWfSentinelPause checks that fireWorkflow consults the
// source-aware paused_jobs row (owner_kind='workflow' since migration 170 retired
// the "__wf__" prefix) — a disabled workflow does not fire, an enabled one does.
func TestFireWorkflowHonorsWfSentinelPause(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at) VALUES ('git', 'workflow', 'wf1', 'op', 't')`); err != nil {
		t.Fatalf("pause: %v", err)
	}

	s := New(pool, quietLog(), nil)
	var fired []string
	s.SetWorkflowFirer(func(_ context.Context, _ string, name, _, _ string) { fired = append(fired, name) })

	s.fireWorkflow("git", "wf1", "default", "") // disabled via pause row → skip
	s.fireWorkflow("git", "wf2", "default", "") // enabled → fire

	if len(fired) != 1 || fired[0] != "wf2" {
		t.Errorf("fired = %v, want [wf2] (wf1 paused via owner_kind=workflow row)", fired)
	}
}

// TestReloadIfChangedGatesOnSHA checks that ReloadIfChanged gates on both
// the synced SHA and the database-level definition fingerprint.
func TestReloadIfChangedGatesOnSHA(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "default", "0 2 * * *", 0)
	if _, err := pool.ExecContext(ctx, `INSERT INTO git_sync_state (id, last_sha) VALUES (1, 'sha1')`); err != nil {
		t.Fatalf("sync state: %v", err)
	}

	s := New(pool, quietLog(), nil)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("after first reload: %d entries, want 1", n)
	}
	if s.reloads != 1 {
		t.Fatalf("reload count = %d, want 1", s.reloads)
	}

	// 1. Same SHA, same DB: must be a no-op.
	s.ReloadIfChanged(ctx, "sha1")
	if s.reloads != 1 {
		t.Fatalf("same-SHA + same-DB reloaded (count = %d, want 1)", s.reloads)
	}

	// 2. Same SHA, changed DB: must reload.
	seedSchedule(t, pool, "job", "j", "extra", "0 */6 * * *", 1)
	s.ReloadIfChanged(ctx, "sha1")
	if s.reloads != 2 {
		t.Fatalf("same-SHA + changed-DB did not reload (count = %d, want 2)", s.reloads)
	}
	if n := len(s.cr.Entries()); n != 2 {
		t.Fatalf("after DB-change reload: %d entries, want 2", n)
	}

	// 3. Changed SHA, same DB: must reload.
	s.ReloadIfChanged(ctx, "sha2")
	if s.reloads != 3 {
		t.Fatalf("changed-SHA + same-DB did not reload (count = %d, want 3)", s.reloads)
	}
}

// TestRebuildWithLocation checks that a zone change swaps the cron engine, sets
// the new location, and re-registers every entry — and that an unchanged zone is
// an idempotent no-op (timezone-update §4.2 / §8).
func TestRebuildWithLocation(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "default", "0 2 * * *", 0)
	seedSchedule(t, pool, "job", "j", "verify", "0 */4 * * *", 1)

	s := New(pool, quietLog(), time.UTC)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.loc.String() != "UTC" {
		t.Fatalf("initial loc = %q, want UTC", s.loc.String())
	}
	if n := len(s.cr.Entries()); n != 2 {
		t.Fatalf("initial entries = %d, want 2", n)
	}
	t.Cleanup(func() { s.cr.Stop() })

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	oldEngine := s.cr
	if err := s.RebuildWithLocation(ctx, ny); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if s.loc.String() != "America/New_York" {
		t.Errorf("after rebuild loc = %q, want America/New_York", s.loc.String())
	}
	if s.cr == oldEngine {
		t.Error("expected a fresh cron engine after a zone change (robfig/cron can't re-zone in place)")
	}
	if n := len(s.cr.Entries()); n != 2 {
		t.Errorf("after rebuild entries = %d, want 2 (all re-registered)", n)
	}

	// Idempotent: rebuilding to the same zone is a no-op (engine unchanged).
	sameEngine := s.cr
	if err := s.RebuildWithLocation(ctx, ny); err != nil {
		t.Fatalf("idempotent rebuild: %v", err)
	}
	if s.cr != sameEngine {
		t.Error("rebuild to the same zone should be a no-op, but the engine was replaced")
	}
}

// TestRebuildWithLocationAdoptsZoneOnReloadFailure pins the §2.2 guarantee that a
// transient reload failure during a zone change never wedges the scheduler: the
// new zone is still adopted and the engine still starts (entries self-heal via the
// backstop), and the call surfaces the error rather than panicking.
func TestRebuildWithLocationAdoptsZoneOnReloadFailure(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), time.UTC)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { s.cr.Stop() })

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	pool.Close() // force the rebuild's entry reload to fail (queries error on a closed pool)

	rerr := s.RebuildWithLocation(ctx, ny)
	if rerr == nil {
		t.Fatal("expected a reload error after closing the pool")
	}
	// The zone is adopted despite the failed entry reload — not wedged in the old zone.
	if s.loc.String() != "America/New_York" {
		t.Errorf("zone not adopted on reload failure: %q", s.loc.String())
	}
}
