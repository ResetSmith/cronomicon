package sshexec

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Phase A of the local runner: the engine is started and stopped at runtime by
// the `localRunner.enabled` setting, and its row in `runners` says which.

func localRunnerEngine(t *testing.T, cfg *config.Config) (*Service, *sql.DB, context.Context) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	if cfg.SecretKEKEnv == "" {
		cfg.SecretKEKEnv = "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="
	}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), t.TempDir())
	wg := &sync.WaitGroup{}
	svc.WithShutdownWG(wg)
	ctx, cancel := context.WithCancel(context.Background())
	// Stop the loops, let in-flight work finalize, THEN close the pool.
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		_ = pool.Close()
	})
	return svc, pool, ctx
}

func localRow(t *testing.T, pool *sql.DB) (id, status string, conc int) {
	t.Helper()
	if err := pool.QueryRow(`SELECT id, status, max_concurrent FROM runners WHERE kind = 'server'`).Scan(&id, &status, &conc); err != nil {
		t.Fatalf("the local runner's row: %v", err)
	}
	return id, status, conc
}

func queueSSHRun(t *testing.T, pool *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT OR IGNORE INTO jobs(name, run_type, command, concurrency_policy, synced_at) VALUES('j-local','bash','true','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, created_at)
		VALUES(?, 'j-local', 'bash', 'queued', 'tester', 'manual', 'ssh', ?)`, id, now); err != nil {
		t.Fatal(err)
	}
}

func runStatus(t *testing.T, pool *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An upgrade arrives in the state it was running in: the SSH executor's old
// switch seeds the setting once, the row is created, and the engine claims.
func TestStartCreatesTheLocalRunnerAndClaimsWhenSeededOn(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{SSHExecutorEnabled: true, SSHExecutorConcurrency: 3})
	queueSSHRun(t, pool, "run-seeded")
	svc.Start(ctx)

	_, status, conc := localRow(t, pool)
	if status != "online" || conc != 3 {
		t.Errorf("after start: status %q, concurrency %d; want online and the seeded 3", status, conc)
	}
	// It has no target to reach, so the run fails — what matters is that it was CLAIMED.
	waitFor(t, "the queued ssh run to be claimed", 5*time.Second, func() bool { return runStatus(t, pool, "run-seeded") != "queued" })
}

// A new install: the row exists, offline, and nothing is claimed.
func TestStartLeavesTheLocalRunnerOffByDefault(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{})
	queueSSHRun(t, pool, "run-waits")
	svc.Start(ctx)
	if _, status, conc := localRow(t, pool); status != "offline" || conc != 4 {
		t.Errorf("status %q, concurrency %d; want offline and the default 4", status, conc)
	}
	time.Sleep(300 * time.Millisecond)
	if s := runStatus(t, pool, "run-waits"); s != "queued" {
		t.Errorf("an ssh run was claimed with the local runner off: %s", s)
	}
}

// The switch works at runtime (LR-43): on starts claiming, off stops, and the
// row's status follows. A change of concurrency is a new bound for the same loop.
func TestApplyStartsAndStopsTheClaimLoop(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{})
	svc.Start(ctx)
	on, off := true, false

	if _, err := settings.SetLocalRunner(ctx, pool, false, &on, nil, "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := localRow(t, pool); status != "online" {
		t.Fatalf("turned on, the row reads %q", status)
	}
	queueSSHRun(t, pool, "run-on")
	waitFor(t, "a run to be claimed after turning on", 5*time.Second, func() bool { return runStatus(t, pool, "run-on") != "queued" })

	six := 6
	if _, err := settings.SetLocalRunner(ctx, pool, false, nil, &six, "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	svc.runMu.Lock()
	running := svc.stopLoop != nil
	svc.runMu.Unlock()
	if conc := svc.bound.Load(); conc != 6 || !running {
		t.Errorf("after a concurrency change the loop runs=%v with a bound of %d, want running with 6", running, conc)
	}

	if _, err := settings.SetLocalRunner(ctx, pool, false, &off, nil, "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := localRow(t, pool); status != "offline" {
		t.Fatalf("turned off, the row reads %q", status)
	}
	// The loop wakes every two seconds; a run queued now must still be queued
	// well after that.
	queueSSHRun(t, pool, "run-off")
	time.Sleep(2500 * time.Millisecond)
	if s := runStatus(t, pool, "run-off"); s != "queued" {
		t.Errorf("a run was claimed after the local runner was turned off: %s", s)
	}
	// Applying an unchanged state is a no-op, not a restart.
	if err := svc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	svc.runMu.Lock()
	running = svc.stopLoop != nil
	svc.runMu.Unlock()
	if running {
		t.Error("an unchanged Apply started the loop")
	}
}

// LR-17: the host's "forbid" wins over a stored "on".
func TestTheHostCanForbidTheLocalRunner(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{SSHExecutorEnabled: true, LocalRunnerForbid: true})
	queueSSHRun(t, pool, "run-forbidden")
	svc.Start(ctx)
	if on, _ := settings.LocalRunnerEnabled(ctx, pool, false); !on {
		t.Fatal("fixture: the stored setting should have been seeded on")
	}
	if _, status, _ := localRow(t, pool); status != "offline" {
		t.Errorf("forbidden on the host, the row reads %q", status)
	}
	time.Sleep(300 * time.Millisecond)
	if s := runStatus(t, pool, "run-forbidden"); s != "queued" {
		t.Errorf("an ssh run was claimed although the host forbids the local runner: %s", s)
	}
}

// The probe-only instance (never started) has nothing to apply.
func TestApplyBeforeStartIsANoOp(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{SSHExecutorEnabled: true})
	if err := svc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&n); err != nil || n != 0 {
		t.Errorf("Apply on an engine that was never started wrote %d runner rows", n)
	}
}

// holdRuns makes every claimed run stay in flight until release is closed, and
// then finish as a success — the loop's own behaviour without a target to dial.
func holdRuns(svc *Service) (release chan struct{}) {
	release = make(chan struct{})
	svc.run = func(ctx context.Context, r claimedRun) {
		<-release
		zero := 0
		svc.finalize(context.WithoutCancel(ctx), r, "success", &zero)
	}
	return release
}

func countRuns(t *testing.T, pool *sql.DB, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status = ?`, status).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setLocal(ctx context.Context, t *testing.T, svc *Service, pool *sql.DB, enabled *bool, conc *int) {
	t.Helper()
	if _, err := settings.SetLocalRunner(ctx, pool, false, enabled, conc, "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
}

// "Runs at once" is the server's bound, not a loop's. Turning the runner off
// and on again while its runs are still finishing must not start a second set
// beside them; a higher bound takes effect without a restart; and a run that
// was in flight when the runner was turned off finishes as itself (LR-43).
func TestTheBoundCountsRunsLeftInFlightByAnEarlierLoop(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{SSHExecutorEnabled: true, SSHExecutorConcurrency: 2})
	release := holdRuns(svc)
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		queueSSHRun(t, pool, id)
	}
	svc.Start(ctx)
	waitFor(t, "two runs in flight", 5*time.Second, func() bool { return countRuns(t, pool, "running") == 2 })

	on, off := true, false
	setLocal(ctx, t, svc, pool, &off, nil)
	if n := countRuns(t, pool, "running"); n != 2 {
		t.Fatalf("turning it off changed what is running: %d, want the same 2", n)
	}
	setLocal(ctx, t, svc, pool, &on, nil)
	// The new loop looks at the queue at once; give it well over one look.
	time.Sleep(600 * time.Millisecond)
	if n := countRuns(t, pool, "running"); n != 2 {
		t.Fatalf("off then on with 2 in flight and a bound of 2: %d running", n)
	}

	three := 3
	setLocal(ctx, t, svc, pool, nil, &three)
	waitFor(t, "a third run once the bound is 3", 5*time.Second, func() bool { return countRuns(t, pool, "running") == 3 })
	time.Sleep(300 * time.Millisecond)
	if n := countRuns(t, pool, "running"); n != 3 {
		t.Fatalf("a bound of 3: %d running", n)
	}

	// Off for good: the three in flight finish as successes, nothing is
	// failed, and the two still queued stay queued.
	setLocal(ctx, t, svc, pool, &off, nil)
	close(release)
	waitFor(t, "the runs in flight to finish", 5*time.Second, func() bool { return countRuns(t, pool, "running") == 0 })
	if ok, queued := countRuns(t, pool, "success"), countRuns(t, pool, "queued"); ok != 3 || queued != 2 {
		t.Errorf("after turning it off: %d succeeded and %d queued, want 3 and 2", ok, queued)
	}
	waitFor(t, "the in-flight count to reach zero", 2*time.Second, func() bool { return svc.inflight.Load() == 0 })
}

// The heartbeat restates the status, so one failed write when the runner was
// turned on does not leave its row reading offline while it claims — and it
// stops with the loop, so it cannot put "online" back after an "off".
func TestTheHeartbeatKeepsTheRowTrue(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 30 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })

	svc, pool, ctx := localRunnerEngine(t, &config.Config{SSHExecutorEnabled: true})
	svc.Start(ctx)
	if _, err := pool.Exec(`UPDATE runners SET status = 'offline', last_seen_at = NULL WHERE kind = 'server'`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the heartbeat to put the row right", 3*time.Second, func() bool {
		_, status, _ := localRow(t, pool)
		return status == "online"
	})
	off := false
	setLocal(ctx, t, svc, pool, &off, nil)
	time.Sleep(200 * time.Millisecond)
	if _, status, _ := localRow(t, pool); status != "offline" {
		t.Errorf("turned off, a heartbeat left the row reading %q", status)
	}
}

// A server that could not set the local runner up at start says so when asked
// to apply a change, instead of answering "applied" for an engine that cannot
// run until the next restart.
func TestApplyReportsAStartThatFailed(t *testing.T) {
	svc, pool, ctx := localRunnerEngine(t, &config.Config{})
	if _, err := pool.Exec(`CREATE TRIGGER no_runners BEFORE INSERT ON runners BEGIN SELECT RAISE(ABORT, 'fixture: no runner rows'); END`); err != nil {
		t.Fatal(err)
	}
	svc.Start(ctx)
	if err := svc.Apply(ctx); err == nil {
		t.Fatal("Apply answered nil although Start could not create the local runner")
	}
}
