package logsync

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/logarchive/fakes3"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// A test harness: migrated SQLite, a fake S3, a store over it, a log dir, and
// an injectable clock.
type harness struct {
	t     *testing.T
	pool  *sql.DB
	fake  *fakes3.Server
	store *logarchive.Store
	dir   string
	now   time.Time
	mu    sync.Mutex
	sw    *Sweeper
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "logsync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	fake := fakes3.New("logs")
	t.Cleanup(fake.Close)
	store, err := logarchive.New(logarchive.Params{
		ClientParams: logarchive.ClientParams{Endpoint: fake.Endpoint(), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"},
		Bucket:       "logs", Prefix: "amadeus/",
	}, httpx.EgressPolicy{AllowPrivate: true, AllowLoopback: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pool: pool, fake: fake, store: store, dir: t.TempDir(), now: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	// backend=s3 so ReadLogSyncSchedule reports Enabled.
	if _, err := pool.Exec(`INSERT INTO log_storage_config(id, backend, local_path, s3_bucket) VALUES(1,'s3',?,'logs')`, h.dir); err != nil {
		t.Fatal(err)
	}
	h.sw = New(Options{
		DB:     pool,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  func() *logarchive.Store { return h.store },
		LogDir: func(context.Context) string { return h.dir },
		Now:    h.clock,
	})
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// run seeds a runs row completed `age` before now, optionally with a log file.
func (h *harness) run(id, code, status string, age time.Duration, content string) {
	h.t.Helper()
	completed := h.clock().Add(-age).Format(time.RFC3339)
	var codeV any
	if code != "" {
		codeV = code
	}
	if _, err := h.pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, completed_at, entity_code)
		VALUES (?, 'job-a', 'bash', ?, 'tester', 'manual', ?, ?, ?)`, id, status, completed, completed, codeV); err != nil {
		h.t.Fatalf("seed run %s: %v", id, err)
	}
	if content != "" {
		dir := h.dir
		if code != "" {
			dir = filepath.Join(h.dir, code)
			_ = os.MkdirAll(dir, 0o750)
			_ = os.WriteFile(filepath.Join(dir, "_meta.json"), []byte(`{"kind":"job","name":"job-a"}`), 0o640)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".log"), []byte(content), 0o640); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) state(id string) (archivedAt, state string) {
	var a, s sql.NullString
	if err := h.pool.QueryRow(`SELECT log_archived_at, log_archive_state FROM runs WHERE id=?`, id).Scan(&a, &s); err != nil {
		h.t.Fatalf("state %s: %v", id, err)
	}
	return a.String, s.String
}

func (h *harness) counters() (count, bytes int64, lastErr string) {
	var e sql.NullString
	if err := h.pool.QueryRow(`SELECT archived_count, archived_bytes, last_sync_error FROM log_storage_config WHERE id=1`).Scan(&count, &bytes, &e); err != nil {
		h.t.Fatal(err)
	}
	return count, bytes, e.String
}

// Trace ids must pass runner.ValidTraceID: use UUID shapes.
const (
	t1 = "0199a000-0000-7000-8000-000000000001"
	t2 = "0199a000-0000-7000-8000-000000000002"
	t3 = "0199a000-0000-7000-8000-000000000003"
	t4 = "0199a000-0000-7000-8000-000000000004"
	t5 = "0199a000-0000-7000-8000-000000000005"
)

func TestTickArchivesPendingAndStampsMarkers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.run(t1, "0a1b2c3d", "success", 5*time.Minute, "one\n")
	h.run(t2, "", "failure", 5*time.Minute, "two two\n")
	h.run(t3, "0a1b2c3d", "skipped", 5*time.Minute, "")       // never had a file
	h.run(t4, "0a1b2c3d", "success", 5*time.Minute, "")       // file gone → missing
	h.run(t5, "0a1b2c3d", "success", 10*time.Second, "fresh") // inside grace → not yet

	res, err := h.sw.RunOnce(ctx, false, "system")
	if err != nil {
		t.Fatalf("RunOnce: %v (%+v)", err, res)
	}
	if res.Uploaded != 2 || res.Missing != 1 || res.Skipped != 1 || res.Failed != 0 || res.Transient != 0 {
		t.Fatalf("result: %+v", res)
	}
	if a, s := h.state(t1); a == "" || s != "" {
		t.Fatalf("t1 = (%q,%q)", a, s)
	}
	if a, s := h.state(t2); a == "" || s != "" {
		t.Fatalf("t2 = (%q,%q)", a, s)
	}
	if a, s := h.state(t3); a != "" || s != "missing" {
		t.Fatalf("skipped t3 = (%q,%q), want missing", a, s)
	}
	if a, s := h.state(t4); a != "" || s != "missing" {
		t.Fatalf("t4 = (%q,%q), want missing", a, s)
	}
	if a, s := h.state(t5); a != "" || s != "" {
		t.Fatalf("t5 inside grace = (%q,%q), want pending", a, s)
	}
	if res.Pending != 1 {
		t.Fatalf("pending = %d, want 1 (t5)", res.Pending)
	}

	// Keys mirror the folder layout; the sidecar rode along; content is exact.
	keys := h.fake.Keys("logs")
	want := []string{"amadeus/" + t2 + ".log", "amadeus/0a1b2c3d/" + t1 + ".log", "amadeus/0a1b2c3d/_meta.json"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if got, _ := h.fake.Get("logs", "amadeus/0a1b2c3d/"+t1+".log"); string(got) != "one\n" {
		t.Fatalf("object content = %q", got)
	}
	count, bytes, lastErr := h.counters()
	if count != 2 || bytes != int64(len("one\n")+len("two two\n")) || lastErr != "" {
		t.Fatalf("counters = (%d,%d,%q)", count, bytes, lastErr)
	}

	// The tick left an activity row; a second, idle tick does not.
	var n int
	_ = h.pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE target='log-archive'`).Scan(&n)
	if n != 1 {
		t.Fatalf("activity rows = %d, want 1", n)
	}
	h.advance(2 * time.Minute) // t5 now past grace
	res, err = h.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Uploaded != 1 {
		t.Fatalf("second tick: %+v %v", res, err)
	}
	res, _ = h.sw.RunOnce(ctx, false, "system")
	if res.Uploaded != 0 || res.Pending != 0 {
		t.Fatalf("idle tick: %+v", res)
	}
	_ = h.pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE target='log-archive'`).Scan(&n)
	if n != 2 {
		t.Fatalf("activity rows after idle tick = %d, want 2", n)
	}
}

func TestTickTransientErrorLeavesPending(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.run(t1, "", "success", 5*time.Minute, "x")
	h.fake.Fail("InternalError", 500)

	res, err := h.sw.RunOnce(ctx, false, "system")
	if err == nil || res.Transient != 1 || res.Uploaded != 0 || res.Aborted {
		t.Fatalf("transient: %+v %v", res, err)
	}
	if a, s := h.state(t1); a != "" || s != "" {
		t.Fatalf("t1 must stay pending, got (%q,%q)", a, s)
	}
	if _, _, lastErr := h.counters(); lastErr == "" {
		t.Fatal("last_sync_error not recorded")
	}

	h.fake.Fail("", 0)
	res, err = h.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Uploaded != 1 {
		t.Fatalf("recovery: %+v %v", res, err)
	}
	if _, _, lastErr := h.counters(); lastErr != "" {
		t.Fatalf("last_sync_error should clear on a clean tick: %q", lastErr)
	}
}

func TestTickPermanentErrorStampsFailedAndAborts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.run(t1, "", "success", 10*time.Minute, "a")
	h.run(t2, "", "success", 5*time.Minute, "b")
	h.fake.Fail("AccessDenied", 403)

	res, err := h.sw.RunOnce(ctx, false, "system")
	if err == nil || !res.Aborted || res.Failed != 1 {
		t.Fatalf("permanent: %+v %v", res, err)
	}
	// Oldest first: t1 took the stamp, t2 was never attempted.
	if _, s := h.state(t1); s != "failed" {
		t.Fatalf("t1 state = %q, want failed", s)
	}
	if a, s := h.state(t2); a != "" || s != "" {
		t.Fatalf("t2 must be untouched, got (%q,%q)", a, s)
	}
	if res.Pending != 1 {
		t.Fatalf("pending = %d, want 1 (t2); failed rows leave the set", res.Pending)
	}
}

func TestTickBatchCapAndBudget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.sw.o.BatchCap = 2
	for _, id := range []string{t1, t2, t3} {
		h.run(id, "", "success", 5*time.Minute, "x")
	}
	res, err := h.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Uploaded != 2 || res.Pending != 1 {
		t.Fatalf("batch cap: %+v %v", res, err)
	}

	// Budget: a clock that jumps past the budget after the first file stops the
	// tick; the remainder carries.
	h2 := newHarness(t)
	h2.sw.o.TickBudget = time.Minute
	for _, id := range []string{t1, t2} {
		h2.run(id, "", "success", 5*time.Minute, "x")
	}
	calls := 0
	h2.sw.o.Now = func() time.Time {
		calls++
		if calls > 3 { // started, cutoff, first deadline check pass; then over budget
			return h2.clock().Add(2 * time.Minute)
		}
		return h2.clock()
	}
	res, err = h2.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Uploaded != 1 || res.Pending != 1 {
		t.Fatalf("budget: %+v %v", res, err)
	}
}

func TestSingleFlight(t *testing.T) {
	h := newHarness(t)
	h.sw.mu.Lock() // simulate a tick in flight
	defer h.sw.mu.Unlock()
	_, err := h.sw.RunOnce(context.Background(), false, "system")
	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("want ErrInProgress, got %v", err)
	}
}

func TestDisabledWhenNoStore(t *testing.T) {
	h := newHarness(t)
	h.sw.o.Store = func() *logarchive.Store { return nil }
	if _, err := h.sw.RunOnce(context.Background(), false, "system"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestReconcileRestoresMarkersAndCounters(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// An object already in the bucket for a pending run (DB restored from an
	// older snapshot), plus a stray non-log key and drifted counters.
	h.run(t1, "0a1b2c3d", "success", 5*time.Minute, "hello")
	h.fake.Put("logs", "amadeus/0a1b2c3d/"+t1+".log", []byte("hello"))
	h.fake.Put("logs", "amadeus/0a1b2c3d/_meta.json", []byte("{}"))
	_, _ = h.pool.Exec(`UPDATE log_storage_config SET archived_count=99, archived_bytes=9999 WHERE id=1`)

	res, err := h.sw.RunOnce(ctx, true, "operator@example.com")
	if err != nil || res.Reconcile != 1 || res.Uploaded != 0 {
		t.Fatalf("reconcile: %+v %v", res, err)
	}
	if a, _ := h.state(t1); a == "" {
		t.Fatal("marker not restored")
	}
	count, bytes, _ := h.counters()
	if count != 1 || bytes != 5 {
		t.Fatalf("counters after reconcile = (%d,%d), want (1,5)", count, bytes)
	}
	var actor string
	_ = h.pool.QueryRow(`SELECT actor FROM activity WHERE target='log-archive' ORDER BY at DESC LIMIT 1`).Scan(&actor)
	if actor != "operator@example.com" {
		t.Fatalf("activity actor = %q", actor)
	}
}

func TestLogDirRepointIsFollowedPerTick(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.run(t1, "", "success", 5*time.Minute, "x")
	// Move the tree, then re-point: the sweep must upload from the new dir.
	newDir := t.TempDir()
	if err := os.Rename(filepath.Join(h.dir, t1+".log"), filepath.Join(newDir, t1+".log")); err != nil {
		t.Fatal(err)
	}
	h.dir = newDir
	res, err := h.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Uploaded != 1 || res.Missing != 0 {
		t.Fatalf("after repoint: %+v %v", res, err)
	}
}

func TestNextDelayAndLoopIdle(t *testing.T) {
	h := newHarness(t)
	sch := func(mode string, interval int, at string, last time.Time) time.Duration {
		return h.sw.nextDelay(settingsSchedule(mode, interval, at, last))
	}
	now := h.clock()
	if d := sch("interval", 900, "", time.Time{}); d != 0 {
		t.Fatalf("never run → now, got %v", d)
	}
	if d := sch("interval", 900, "", now.Add(-5*time.Minute)); d != 10*time.Minute {
		t.Fatalf("interval remaining = %v, want 10m", d)
	}
	if d := sch("interval", 900, "", now.Add(-20*time.Minute)); d != 0 {
		t.Fatalf("overdue interval → now, got %v", d)
	}
	if d := sch("daily", 0, "13:30", now.Add(-time.Hour)); d != 90*time.Minute {
		t.Fatalf("daily until 13:30 from 12:00 = %v, want 1h30m", d)
	}
	if d := sch("daily", 0, "13:30", now.Add(-25*time.Hour)); d != 0 {
		t.Fatalf("daily overdue → now, got %v", d)
	}

	// The loop: with the backend local it idles on Kick and never ticks.
	_, _ = h.pool.Exec(`UPDATE log_storage_config SET backend='local' WHERE id=1`)
	h.run(t1, "", "success", 5*time.Minute, "x")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	h.sw.o.WG = &wg
	h.sw.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	if a, _ := h.state(t1); a != "" {
		t.Fatal("loop ticked while backend was local")
	}
	// Enable + kick: the loop re-reads, sees never-run, ticks at once.
	_, _ = h.pool.Exec(`UPDATE log_storage_config SET backend='s3' WHERE id=1`)
	h.sw.Kick()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a, _ := h.state(t1); a != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("loop did not tick after kick")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
}

func settingsSchedule(mode string, interval int, at string, last time.Time) settings.LogSyncSchedule {
	return settings.LogSyncSchedule{Enabled: true, Mode: mode, IntervalSeconds: interval, At: at, LastFinishedAt: last}
}

// SL-4 — the archive tier's own window. 0 never deletes; a window deletes the
// object, clears the marker, decrements the counters, and removes a folder's
// sidecar once its last log is gone. A refused delete stops the pass and is
// the tick's error; the marker stays so nothing is lost.
func TestExpireArchived(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setWindow := func(days int) {
		ac, _ := settings.GetAuditCompliance(ctx, h.pool)
		ac.RetentionDays.ArchivedLogFiles = days
		if _, err := settings.UpdateAuditCompliance(ctx, h.pool, *ac, "t"); err != nil {
			t.Fatal(err)
		}
	}
	h.run(t1, "0a1b2c3d", "success", 5*time.Minute, "old\n")
	h.run(t2, "0a1b2c3d", "success", 5*time.Minute, "new\n")
	if res, err := h.sw.RunOnce(ctx, false, "system"); err != nil || res.Uploaded != 2 {
		t.Fatalf("seed archive: %+v %v", res, err)
	}
	// Age t1's marker past a 30-day window; t2 stays fresh.
	if _, err := h.pool.Exec(`UPDATE runs SET log_archived_at=? WHERE id=?`, h.clock().AddDate(0, 0, -40).Format(time.RFC3339), t1); err != nil {
		t.Fatal(err)
	}

	// Window 0: nothing expires.
	res, err := h.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Expired != 0 {
		t.Fatalf("window 0: %+v %v", res, err)
	}
	if _, ok := h.fake.Get("logs", "amadeus/0a1b2c3d/"+t1+".log"); !ok {
		t.Fatal("window 0 must never delete")
	}

	setWindow(30)
	res, err = h.sw.RunOnce(ctx, false, "system")
	if err != nil || res.Expired != 1 {
		t.Fatalf("window 30: %+v %v", res, err)
	}
	if _, ok := h.fake.Get("logs", "amadeus/0a1b2c3d/"+t1+".log"); ok {
		t.Fatal("expired object still in the bucket")
	}
	if a, s := h.state(t1); a != "" || s != "expired" {
		t.Fatalf("expired run should be stamped expired (out of the pending set), got (%q,%q)", a, s)
	}
	if a, _ := h.state(t2); a == "" {
		t.Fatal("fresh run lost its marker")
	}
	count, bytes, _ := h.counters()
	if count != 1 || bytes != int64(len("new\n")) {
		t.Fatalf("counters after expiry = (%d,%d)", count, bytes)
	}
	if _, ok := h.fake.Get("logs", "amadeus/0a1b2c3d/_meta.json"); !ok {
		t.Fatal("sidecar must stay while the folder still holds a log")
	}

	// Expire the last log in the folder: the sidecar goes with it.
	if _, err := h.pool.Exec(`UPDATE runs SET log_archived_at=? WHERE id=?`, h.clock().AddDate(0, 0, -40).Format(time.RFC3339), t2); err != nil {
		t.Fatal(err)
	}
	if res, err := h.sw.RunOnce(ctx, false, "system"); err != nil || res.Expired != 1 {
		t.Fatalf("expire last: %+v %v", res, err)
	}
	if keys := h.fake.Keys("logs"); len(keys) != 0 {
		t.Fatalf("folder should be empty including the sidecar, got %v", keys)
	}

	// A refused delete: marker kept, tick reports the error.
	h.run(t3, "", "success", 5*time.Minute, "z")
	if _, err := h.sw.RunOnce(ctx, false, "system"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(`UPDATE runs SET log_archived_at=? WHERE id=?`, h.clock().AddDate(0, 0, -40).Format(time.RFC3339), t3); err != nil {
		t.Fatal(err)
	}
	h.fake.Fail("AccessDenied", 403)
	res, err = h.sw.RunOnce(ctx, false, "system")
	if err == nil || res.Expired != 0 {
		t.Fatalf("refused delete: %+v %v", res, err)
	}
	if a, _ := h.state(t3); a == "" {
		t.Fatal("marker must survive a refused delete")
	}
}
