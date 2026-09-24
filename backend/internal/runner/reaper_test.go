package runner

import (
	"context"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"testing"
	"time"
)

// withReaperNow swaps the reaperNow clock seam for the duration of fn and
// restores it afterwards.
func withReaperNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := reaperNow
	reaperNow = func() time.Time { return at }
	t.Cleanup(func() { reaperNow = prev })
}

// insertRunnerWithHeartbeat inserts a runner row with an explicit last_seen_at.
func insertRunnerWithHeartbeat(t *testing.T, svc *Service, id, name, status, lastSeenAt, registeredAt string) {
	t.Helper()
	// protocol_version: the current one, for the same reason insertRunner seeds
	// it — a runners row exists only because registration created it.
	_, err := svc.db.Exec(`
		INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, last_seen_at, registered_at, created_at)
		VALUES (?, ?, ?, 'Linux', '["bash"]', 0, 5, '1.0', ?, ?, ?, ?)`,
		id, name, status, runnerproto.ProtocolVersion, nullIf(lastSeenAt), registeredAt, registeredAt)
	if err != nil {
		t.Fatalf("insertRunnerWithHeartbeat: %v", err)
	}
}

// insertRunningRun inserts a run already in 'running' on the given runner.
func insertRunningRun(t *testing.T, svc *Service, traceID, jobName, scope, runnerID, startedAt string) {
	t.Helper()
	ts := now()
	_, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, created_at, started_at)
		VALUES (?, ?, 'bash', ?, 'running', ?, 'test', 'manual', ?, ?)`,
		traceID, jobName, scope, runnerID, ts, startedAt)
	if err != nil {
		t.Fatalf("insertRunningRun: %v", err)
	}
}

func runnerStatus(t *testing.T, svc *Service, id string) (status string, exists bool) {
	t.Helper()
	err := svc.db.QueryRow(`SELECT status FROM runners WHERE id = ?`, id).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

// TestReaperOfflinesStaleRunnerAndReconcilesRun covers the core R3 exit
// criteria: a stale runner → offline; its running run → failure/runner_lost
// with the terminal seam fired; a fresh runner stays online.
func TestReaperOfflinesStaleRunnerAndReconcilesRun(t *testing.T) {
	svc := newTestService(t)
	cap := &captureNotifier{}
	svc.WithNotifier(cap)
	ctx := context.Background()

	// Anchor "now" so heartbeat ages are deterministic. Default offline window 5m.
	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	svc.cfg.RunnerOfflineAfter = 5 * time.Minute
	svc.cfg.RunnerDeregisterAfter = 336 * time.Hour

	// Stale runner: last heartbeat 10m ago (> 5m window) → should go offline.
	staleSeen := base.Add(-10 * time.Minute).Format(time.RFC3339)
	startedAt := base.Add(-9 * time.Minute).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, "stale-runner", "stale", "online", staleSeen, staleSeen)
	insertRunningRun(t, svc, "orphan-run", "nightly", "prod", "stale-runner", startedAt)

	// Fresh runner: heartbeat 1m ago (< 5m) → should stay online.
	freshSeen := base.Add(-1 * time.Minute).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, "fresh-runner", "fresh", "online", freshSeen, freshSeen)

	withReaperNow(t, base)
	svc.reapOnce(ctx)

	// Stale runner is offline.
	if st, _ := runnerStatus(t, svc, "stale-runner"); st != "offline" {
		t.Errorf("stale runner status = %q, want offline", st)
	}
	// Fresh runner stays online.
	if st, _ := runnerStatus(t, svc, "fresh-runner"); st != "online" {
		t.Errorf("fresh runner status = %q, want online", st)
	}

	// Orphaned run reconciled to failure / runner_lost, completed + duration set.
	var status, reason, completedAt string
	var durationMs int64
	if err := svc.db.QueryRow(`
		SELECT status, COALESCE(queued_reason,''), COALESCE(completed_at,''), COALESCE(duration_ms,0)
		FROM runs WHERE id = 'orphan-run'`).Scan(&status, &reason, &completedAt, &durationMs); err != nil {
		t.Fatalf("query orphan run: %v", err)
	}
	if status != "failure" || reason != "runner_lost" {
		t.Errorf("orphan run status=%q reason=%q, want failure/runner_lost", status, reason)
	}
	if completedAt == "" {
		t.Error("orphan run completed_at not set")
	}
	if durationMs <= 0 {
		t.Errorf("orphan run duration_ms = %d, want > 0 (computed from started_at)", durationMs)
	}

	// Terminal seam fired exactly once for the orphaned run.
	if len(cap.events) != 1 {
		t.Fatalf("expected 1 RunEnded event, got %d", len(cap.events))
	}
	if ev := cap.events[0]; ev.TraceID != "orphan-run" || ev.Status != "failure" || ev.JobName != "nightly" {
		t.Errorf("unexpected run-end event: %+v", ev)
	}

	// run-end activity row written by the system actor.
	var actCount int
	_ = svc.db.QueryRow(`
		SELECT COUNT(1) FROM activity WHERE kind='run-end' AND trace_id='orphan-run' AND actor='system'`).
		Scan(&actCount)
	if actCount != 1 {
		t.Errorf("expected 1 system run-end activity, got %d", actCount)
	}
}

// TestReaperDeregistersLongOfflineRunner covers the D4 deregister sweep: a
// runner offline past the deregister window is removed from runners, and its
// tokens are revoked.
func TestReaperDeregistersLongOfflineRunner(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	svc.cfg.RunnerOfflineAfter = 5 * time.Minute
	svc.cfg.RunnerDeregisterAfter = 336 * time.Hour // 14d

	// Offline runner last seen 15 days ago (> 14d) → deregistered.
	oldSeen := base.Add(-15 * 24 * time.Hour).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, "old-offline", "ancient", "offline", oldSeen, oldSeen)
	// Give it a token so we can assert revocation.
	if _, err := svc.db.Exec(`
		INSERT INTO runner_tokens(token_hash, created_by, created_at, expires_at)
		VALUES ('hash-old', 'runner:ancient', ?, ?)`,
		oldSeen, base.Add(365*24*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	// Offline runner last seen 3 days ago (< 14d) → kept.
	recentSeen := base.Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, "recent-offline", "recent", "offline", recentSeen, recentSeen)

	withReaperNow(t, base)
	svc.reapOnce(ctx)

	// Old offline runner is gone.
	if _, ok := runnerStatus(t, svc, "old-offline"); ok {
		t.Error("expected old-offline runner to be deregistered (deleted)")
	}
	// Its token is revoked.
	var revoked *string
	_ = svc.db.QueryRow(`SELECT revoked_at FROM runner_tokens WHERE token_hash='hash-old'`).Scan(&revoked)
	if revoked == nil {
		t.Error("expected old runner's token to be revoked")
	}

	// Recent offline runner is kept.
	if st, ok := runnerStatus(t, svc, "recent-offline"); !ok || st != "offline" {
		t.Errorf("recent-offline runner = %q (exists=%v), want offline/kept", st, ok)
	}
}

// TestReaperNullHeartbeatFallsBackToRegisteredAt verifies a runner that never
// polled (NULL last_seen_at) but registered long ago is still reaped, using
// registered_at as the heartbeat fallback.
func TestReaperNullHeartbeatFallsBackToRegisteredAt(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	svc.cfg.RunnerOfflineAfter = 5 * time.Minute
	svc.cfg.RunnerDeregisterAfter = 336 * time.Hour

	// Registered 10m ago, never polled (NULL last_seen_at).
	registeredAt := base.Add(-10 * time.Minute).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, "never-polled", "ghost", "online", "", registeredAt)

	withReaperNow(t, base)
	svc.reapOnce(ctx)

	if st, _ := runnerStatus(t, svc, "never-polled"); st != "offline" {
		t.Errorf("never-polled runner status = %q, want offline (registered_at fallback)", st)
	}
}
