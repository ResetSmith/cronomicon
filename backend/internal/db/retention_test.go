package db

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/metrics"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func migratedPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := Open(filepath.Join(t.TempDir(), "ret.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestUntilNext verifies the wall-clock re-arm math (PP-H6): the duration to the
// next HH:MM UTC, rolling to tomorrow once today's time has passed.
func TestUntilNext(t *testing.T) {
	now := time.Date(2026, 6, 16, 1, 0, 0, 0, time.UTC)
	if d := untilNext("02:00", now); d != time.Hour {
		t.Errorf("untilNext(02:00, 01:00) = %v, want 1h", d)
	}
	if d := untilNext("02:00", time.Date(2026, 6, 16, 3, 0, 0, 0, time.UTC)); d != 23*time.Hour {
		t.Errorf("untilNext(02:00, 03:00) = %v, want 23h (next day)", d)
	}
	// Empty falls back to defaultBackupAt (02:00).
	if d := untilNext("", now); d != time.Hour {
		t.Errorf("untilNext(\"\", 01:00) = %v, want 1h (default 02:00)", d)
	}
}

// TestBackupOverdue verifies the boot-catch-up gate (PP-H6): overdue when never
// run or older than the interval; not overdue when recent.
func TestBackupOverdue(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	if !backupOverdue(ctx, pool, backupInterval) {
		t.Error("fresh DB should be overdue (no prior backup)")
	}
	persistLastSuccess(ctx, pool, nowUTC())
	if backupOverdue(ctx, pool, backupInterval) {
		t.Error("just-backed-up DB should NOT be overdue")
	}
	persistLastSuccess(ctx, pool, nowUTC().Add(-48*time.Hour))
	if !backupOverdue(ctx, pool, backupInterval) {
		t.Error("48h-old backup should be overdue")
	}
}

// TestStartRetentionBootCatchup verifies the boot sweep runs when overdue and is
// skipped when a recent backup exists (PP-H6 crash-loop guard).
func TestStartRetentionBootCatchup(t *testing.T) {
	t.Run("overdue runs", func(t *testing.T) {
		pool := migratedPool(t)
		var uploaded atomic.Int32
		ctx := t.Context()
		StartRetention(ctx, pool, RetentionPolicy{
			BackupDir: t.TempDir(),
			Upload:    func(context.Context, string) error { uploaded.Add(1); return nil },
		}, testLogger())
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && uploaded.Load() == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if uploaded.Load() == 0 {
			t.Error("boot catch-up did not run when overdue")
		}
	})

	t.Run("recent skips", func(t *testing.T) {
		pool := migratedPool(t)
		persistLastSuccess(context.Background(), pool, nowUTC())
		var uploaded atomic.Int32
		ctx := t.Context()
		StartRetention(ctx, pool, RetentionPolicy{
			BackupDir: t.TempDir(),
			Upload:    func(context.Context, string) error { uploaded.Add(1); return nil },
		}, testLogger())
		time.Sleep(500 * time.Millisecond)
		if uploaded.Load() != 0 {
			t.Errorf("boot catch-up ran %d times despite recent backup", uploaded.Load())
		}
	})
}

// TestRunSweepPersistsLastSuccess verifies a successful sweep records the
// last-success timestamp (PP-H6 H6-3).
func TestRunSweepPersistsLastSuccess(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	if !backupOverdue(ctx, pool, backupInterval) {
		t.Fatal("precondition: fresh DB overdue")
	}
	if err := runSweep(ctx, pool, RetentionPolicy{BackupDir: t.TempDir()}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if backupOverdue(ctx, pool, backupInterval) {
		t.Error("last-success not persisted after a successful sweep")
	}
}

// TestSweepIdempotentSameDay is the PP-M2 regression: a second sweep on the same
// UTC day succeeds (the snapshot write is idempotent) rather than aborting on a
// VACUUM-INTO overwrite refusal after the prune already ran.
func TestSweepIdempotentSameDay(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()

	orig := nowUTC
	defer func() { nowUTC = orig }()
	fixed := time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	nowUTC = func() time.Time { mu.Lock(); defer mu.Unlock(); return fixed }

	policy := RetentionPolicy{BackupDir: dir}
	if err := runSweep(context.Background(), pool, policy, testLogger()); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if err := runSweep(context.Background(), pool, policy, testLogger()); err != nil {
		t.Fatalf("second same-day sweep failed (PP-M2 overwrite not handled): %v", err)
	}
	snap := filepath.Join(dir, "cronomicon-20260616.db")
	if _, err := Open(snap); err != nil {
		t.Fatalf("same-day snapshot missing/invalid after second sweep: %v", err)
	}
}

// TestRetentionSweepBackup verifies the A4 nightly path: the sweep writes a
// VACUUM-INTO snapshot to the backup dir and invokes the upload callback with it.
func TestRetentionSweepBackup(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "ret.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	backupDir := t.TempDir()
	var uploaded string
	policy := RetentionPolicy{
		RunsDays:      90,
		ChangeLogDays: 365,
		BackupDir:     backupDir,
		Upload: func(_ context.Context, localPath string) error {
			uploaded = localPath
			return nil
		},
	}

	if err := runSweep(context.Background(), pool, policy, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if uploaded == "" {
		t.Fatal("upload callback was not invoked")
	}
	if filepath.Dir(uploaded) != backupDir {
		t.Fatalf("snapshot written to %s, want under %s", uploaded, backupDir)
	}
	// The snapshot file must exist and be a valid SQLite DB (openable).
	snap, err := Open(uploaded)
	if err != nil {
		t.Fatalf("snapshot not a valid db: %v", err)
	}
	snap.Close()
}

// ── LU-1: on-disk run-log reaper ──────────────────────────────────────────────
//
// Why these matter: until LU-1 nothing ever removed {LogDir}/*.log, so the log
// volume grew without bound (G1) — including logs whose owning runs row had
// already been pruned by the same sweep. The reaper closes that gap, but it
// deletes files on an operator-configurable path, so its *restraint* (suffix
// filter, "0 = keep forever", empty-dir handling, never failing the sweep)
// carries at least as much weight as its deletions.

// writeLogFileAged creates path (making parents) and back-dates its mtime by
// age. mtime is what the reaper judges, so this is the whole fixture.
func writeLogFileAged(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("log body\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s: %s is gone (%v)", why, path, err)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s: %s still exists", why, path)
	}
}

// backupFailuresTotal scrapes the process-wide registry through its public
// /metrics handler and returns cronomicon_backup_failures_total. The counter field
// is unexported and there is no getter, but the handler is the same surface
// monitoring reads — so this observes exactly what an alert would see, without
// adding a test-only hook to non-test code.
func backupFailuresTotal(t *testing.T) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Default().Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "cronomicon_backup_failures_total ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "cronomicon_backup_failures_total ")), 64)
		if err != nil {
			t.Fatalf("parse backup failures counter from %q: %v", line, err)
		}
		return v
	}
	t.Fatal("cronomicon_backup_failures_total not present in /metrics output")
	return 0
}

// TestSweepReapsAgedLogsAndKeepsFreshOnes proves the reaper is wired into the
// sweep and that the cutoff is honoured in both directions: past the window the
// file goes, inside the window it stays. A reaper that only got the first half
// right would silently delete logs an operator is still allowed to read.
func TestSweepReapsAgedLogsAndKeepsFreshOnes(t *testing.T) {
	pool := migratedPool(t)
	logDir := t.TempDir()
	aged := filepath.Join(logDir, "aged.log")
	fresh := filepath.Join(logDir, "fresh.log")
	writeLogFileAged(t, aged, 10*24*time.Hour)
	writeLogFileAged(t, fresh, 1*time.Hour)

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		LogDir: logDir, LogFilesDays: 7, BackupDir: t.TempDir(),
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	mustNotExist(t, aged, "a log 10d past a 7d window must be reaped")
	mustExist(t, fresh, "a log inside the retention window must survive")
}

// TestReapLogFilesZeroDaysKeepsForever pins the "0 = keep forever" convention
// the day knobs share (RetentionPolicy doc). A fresh deployment leaves
// LogFilesDays unset, so if zero meant "cutoff = now" the very first sweep after
// upgrading would delete every run log on the box.
func TestReapLogFilesZeroDaysKeepsForever(t *testing.T) {
	logDir := t.TempDir()
	ancient := filepath.Join(logDir, "ancient.log")
	writeLogFileAged(t, ancient, 5000*24*time.Hour)

	for _, days := range []int{0, -1} {
		reapLogFiles(logDir, days, nil, nil, testLogger())
		mustExist(t, ancient, "LogFilesDays="+strconv.Itoa(days)+" must keep everything forever")
	}
}

// TestReapLogFilesEmptyDirIsNoop covers the degrade path: settings.ResolveLogDir
// failing leaves LogDir empty, and the reaper must then do nothing at all rather
// than default to a directory (or to "" ⇒ the process CWD) and sweep the wrong
// tree. It must also not panic, since it runs inside the nightly sweep.
func TestReapLogFilesEmptyDirIsNoop(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reapLogFiles(\"\") panicked: %v", r)
		}
	}()
	// A file in the working directory must be untouched by a "" log dir.
	guard := filepath.Join(t.TempDir(), "guard.log")
	writeLogFileAged(t, guard, 90*24*time.Hour)
	reapLogFiles("", 1, nil, nil, testLogger())
	mustExist(t, guard, "an empty LogDir must reap nothing anywhere")
}

// TestReapLogFilesOnlyTouchesDotLog is the safety property that makes the whole
// feature acceptable: the log directory is operator-configurable, so a mistyped
// path can point the reaper at a data directory. The *.log suffix filter is the
// only thing standing between a typo and an aged cronomicon.db being deleted on the
// next nightly sweep — the same discipline the snapshot GC applies with
// cronomicon-*.db. Every non-.log file here is aged far past the cutoff and MUST
// survive.
func TestReapLogFilesOnlyTouchesDotLog(t *testing.T) {
	logDir := t.TempDir()
	survivors := []string{
		filepath.Join(logDir, "cronomicon.db"),     // the live database, if the path were mistyped
		filepath.Join(logDir, "cronomicon.db-wal"), // …and its sidecars
		filepath.Join(logDir, "notes.txt"),         // an operator's own file
		filepath.Join(logDir, "run.log.gz"),        // an externally rotated log
		filepath.Join(logDir, "log"),               // suffix-lookalike without the dot
		filepath.Join(logDir, "nested", "a.json"),
	}
	for _, p := range survivors {
		writeLogFileAged(t, p, 400*24*time.Hour)
	}
	victim := filepath.Join(logDir, "aged.log")
	writeLogFileAged(t, victim, 400*24*time.Hour)

	reapLogFiles(logDir, 30, nil, nil, testLogger())

	for _, p := range survivors {
		mustExist(t, p, "non-.log file must NEVER be reaped, however aged")
	}
	mustNotExist(t, victim, "the aged .log itself should still be reaped")
}

// TestReapLogFilesWalksSubdirectories pins the walk as recursive. Today's layout
// is flat, but LU-7 moves logs into per-entity folders; a top-level-only reaper
// would then quietly stop collecting anything and the G1 growth would return
// unnoticed because the code still "works".
func TestReapLogFilesWalksSubdirectories(t *testing.T) {
	logDir := t.TempDir()
	nested := filepath.Join(logDir, "jobs", "nightly-backup", "aged.log")
	nestedFresh := filepath.Join(logDir, "jobs", "nightly-backup", "fresh.log")
	writeLogFileAged(t, nested, 60*24*time.Hour)
	writeLogFileAged(t, nestedFresh, time.Hour)

	reapLogFiles(logDir, 30, nil, nil, testLogger())

	mustNotExist(t, nested, "an aged .log in a subdirectory must be reaped (LU-7 forward-compat)")
	mustExist(t, nestedFresh, "a fresh .log in a subdirectory must survive")
}

// TestReapLogFilesPrunesEmptyDirsButNotRoot covers the cleanup half: emptied
// per-entity folders must not accumulate as directory litter, yet the root log
// dir has to survive — the next run writes into it, and re-creating it would
// lose the mount's ownership/permissions. A directory still holding a live log
// must also survive, so an in-flight run's folder is never yanked out from under
// it.
func TestReapLogFilesPrunesEmptyDirsButNotRoot(t *testing.T) {
	logDir := t.TempDir()
	emptied := filepath.Join(logDir, "jobs", "gone")
	kept := filepath.Join(logDir, "jobs", "active")
	writeLogFileAged(t, filepath.Join(emptied, "aged.log"), 60*24*time.Hour)
	writeLogFileAged(t, filepath.Join(kept, "fresh.log"), time.Hour)
	// A directory that was already empty before the sweep is litter too.
	if err := os.MkdirAll(filepath.Join(logDir, "stale-empty"), 0o750); err != nil {
		t.Fatalf("mkdir stale-empty: %v", err)
	}

	reapLogFiles(logDir, 30, nil, nil, testLogger())

	mustExist(t, logDir, "the root log dir must NEVER be removed")
	mustExist(t, kept, "a directory still holding a fresh log must survive")
	mustExist(t, filepath.Join(kept, "fresh.log"), "the fresh log itself must survive")
	mustNotExist(t, emptied, "a directory emptied by the reaper must be pruned")
	mustNotExist(t, filepath.Join(logDir, "stale-empty"), "an already-empty directory must be pruned")
	// The parent is collected in the same pass (deepest-first ordering) only if
	// it too is now empty; here "active" remains, so "jobs" must stay.
	mustExist(t, filepath.Join(logDir, "jobs"), "a parent with a surviving child must stay")
}

// TestSweepSurvivesBrokenLogDir is the containment property: the log reaper is a
// best-effort janitor bolted onto the nightly backup, so a bad log path (missing
// mount, unreadable directory) must not abort the sweep before it writes the
// snapshot. The backup is the load-bearing half of runSweep; losing it to a
// cosmetic log problem would be a strictly worse trade than never reaping.
func TestSweepSurvivesBrokenLogDir(t *testing.T) {
	pool := migratedPool(t)
	backupDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no", "such", "log", "dir")

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		LogDir: missing, LogFilesDays: 7, BackupDir: backupDir,
	}, testLogger()); err != nil {
		t.Fatalf("sweep aborted on an unreadable log dir: %v", err)
	}
	if backupOverdue(context.Background(), pool, backupInterval) {
		t.Error("the backup half of the sweep did not complete despite the log dir being the only problem")
	}
}

// TestBrokenLogDirDoesNotTripBackupFailedMetric is the regression that keeps the
// alerting honest. cronomicon_backup_failures_total drives the "backups are broken"
// alert; if a missing log directory incremented it, on-call would be paged for a
// backup that in fact succeeded, and the alert would be trained into noise. This
// is why reapLogFiles returns no error at all: runSweep's deferred hook fires the
// counter on any non-context error it returns.
func TestBrokenLogDirDoesNotTripBackupFailedMetric(t *testing.T) {
	pool := migratedPool(t)
	before := backupFailuresTotal(t)

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		LogDir:       filepath.Join(t.TempDir(), "definitely", "missing"),
		LogFilesDays: 7,
		BackupDir:    t.TempDir(),
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if after := backupFailuresTotal(t); after != before {
		t.Errorf("cronomicon_backup_failures_total went %v → %v: a log-dir problem must not be reported as a backup failure", before, after)
	}
}

// ── LU-2: per-sweep policy reload + independent per-table knobs ───────────────

// seedRun inserts a runs row with an explicit created_at so retention cutoffs
// can be exercised without waiting.
func seedRun(t *testing.T, pool *sql.DB, id string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
		VALUES (?, 'job-a', 'bash', 'success', 'tester', 'manual', ?)`,
		id, nowUTC().Add(-age).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
}

// seedActivity inserts an activity row with an explicit created_at.
func seedActivity(t *testing.T, pool *sql.DB, age time.Duration) {
	t.Helper()
	ts := nowUTC().Add(-age).Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO activity(kind, summary, at, created_at) VALUES ('run-end', 'x', ?, ?)`,
		ts, ts); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
}

func countRows(t *testing.T, pool *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil { //nolint:gosec // literal table names
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestSweepUsesReloadedPolicyNotBootPolicy pins LU-2's whole point: the sweep
// prunes to the CURRENTLY configured windows, not the ones captured at boot.
// Without the Reload hook a retention change made in the settings panel would
// sit inert until someone restarted the process — the kind of silent no-op that
// makes an operator distrust the panel. The boot policy here says "keep runs
// forever"; the reloaded one says 1 day, and the aged row must be gone.
func TestSweepUsesReloadedPolicyNotBootPolicy(t *testing.T) {
	pool := migratedPool(t)
	seedRun(t, pool, "run-aged", 10*24*time.Hour)
	seedRun(t, pool, "run-fresh", time.Hour)

	var reloaded atomic.Int32
	boot := RetentionPolicy{
		RunsDays:  0, // boot policy: keep forever
		BackupDir: t.TempDir(),
		Reload: func(_ context.Context, p RetentionPolicy) RetentionPolicy {
			reloaded.Add(1)
			p.RunsDays = 1 // operator changed it after boot
			return p
		},
	}
	if err := runSweep(context.Background(), pool, boot, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if reloaded.Load() != 1 {
		t.Fatalf("Reload called %d times, want exactly 1 per sweep", reloaded.Load())
	}
	if n := countRows(t, pool, "runs"); n != 1 {
		t.Fatalf("runs count = %d, want 1: the sweep used the boot policy (RunsDays=0) instead of the reloaded one", n)
	}
	var id string
	if err := pool.QueryRow(`SELECT id FROM runs`).Scan(&id); err != nil {
		t.Fatalf("read surviving run: %v", err)
	}
	if id != "run-fresh" {
		t.Errorf("surviving run = %q, want run-fresh", id)
	}
}

// TestPerTableRetentionKnobsAreIndependent is the LU-2 regression proper: before
// it, two fields covered five tables, so three of the six knobs in the Audit &
// Compliance panel had nothing to map onto and were inert (G2). Pruning activity
// on a 1-day window must not touch runs when runs is set to keep forever.
func TestPerTableRetentionKnobsAreIndependent(t *testing.T) {
	pool := migratedPool(t)
	seedRun(t, pool, "run-aged", 10*24*time.Hour)
	seedActivity(t, pool, 10*24*time.Hour)
	seedActivity(t, pool, time.Hour)

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		ActivityDays: 1,
		RunsDays:     0, // keep forever
		BackupDir:    t.TempDir(),
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if n := countRows(t, pool, "activity"); n != 1 {
		t.Errorf("activity count = %d, want 1 (ActivityDays=1 should prune the aged row)", n)
	}
	if n := countRows(t, pool, "runs"); n != 1 {
		t.Errorf("runs count = %d, want 1: RunsDays=0 means keep forever, so the activity knob must not reach runs", n)
	}
}

// TestReapLogFilesSpareKeepFiles is what stops the nightly janitor deleting the
// live process log out from under its own open handle.
//
// The process log (LU-4) lives in the same tree as the run logs and ends in .log
// like all of them, but it is held open for the whole life of the process and is
// rotated on its own keep-count. Reaping it would be doubly wrong: the space is
// not freed (the open descriptor keeps the inode alive until the process exits)
// and every subsequent log line is written to a file no longer reachable by
// name — so the server appears to be logging while the operator's `tail` shows
// nothing, which is the exact failure the file was added to prevent. And the
// mtime of an appended-to log makes it look ancient only if the box is quiet,
// so this would strike hardest on the idle systems nobody is watching.
func TestReapLogFilesSpareKeepFiles(t *testing.T) {
	logDir := t.TempDir()
	processLog := filepath.Join(logDir, "cronomicon.log")
	runLog := filepath.Join(logDir, "old-run.log")
	// Equally aged, equally .log, in the same directory: the ONLY thing that
	// may distinguish them is the keep list.
	writeLogFileAged(t, processLog, 90*24*time.Hour)
	writeLogFileAged(t, runLog, 90*24*time.Hour)

	reapLogFiles(logDir, 30, []string{"cronomicon.log"}, nil, testLogger())

	mustExist(t, processLog, "a file named in KeepFiles must survive the reap however aged — it is held open by the writer")
	mustNotExist(t, runLog, "an equally aged sibling not in KeepFiles must still be reaped")
}

// TestSweepPassesKeepFilesThrough is the plumbing half: KeepFiles is set once, in
// main's RetentionPolicy, and travels through runSweep to the reaper. A unit test
// on reapLogFiles alone would keep passing if the policy field were never read,
// which is precisely how the live process log would get deleted in production
// while the test suite stayed green.
func TestSweepPassesKeepFilesThrough(t *testing.T) {
	pool := migratedPool(t)
	logDir := t.TempDir()
	processLog := filepath.Join(logDir, "cronomicon.log")
	runLog := filepath.Join(logDir, "aged-run.log")
	writeLogFileAged(t, processLog, 90*24*time.Hour)
	writeLogFileAged(t, runLog, 90*24*time.Hour)

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		LogDir:       logDir,
		LogFilesDays: 7,
		KeepFiles:    []string{"cronomicon.log"},
		BackupDir:    t.TempDir(),
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	mustExist(t, processLog, "RetentionPolicy.KeepFiles must reach the reaper through runSweep")
	mustNotExist(t, runLog, "the sweep must still reap aged run logs")
}

// ── LU-10: the audit stream's own retention window ────────────────────────────
//
// Why this block exists: audit.log's dated generations are compliance evidence
// with a retention window measured in years, and they sit in the same directory
// as run logs whose window is measured in weeks. Everything below is about the
// separation between those two — a reaper that over-matches here deletes exactly
// what an auditor came for, and nothing in the product would report it missing.

// writeAuditGeneration creates the `<base>.YYYYMMDD` sibling for a day that many
// days in the past. The reaper judges the NAME, not the mtime — a generation is
// named for the day whose records it holds — so the name is the whole fixture.
func writeAuditGeneration(t *testing.T, livePath string, daysAgo int) string {
	t.Helper()
	day := nowUTC().AddDate(0, 0, -daysAgo).Format("20060102")
	p := livePath + "." + day
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(`{"v":1,"at":"x","source":"auth"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestReapAuditLogRemovesAgedGenerationsAndNeverTheLiveFile is the core of the
// reaper in both directions. Past the window a generation must go, inside it a
// generation must stay, and the LIVE file must survive regardless of age: it is
// held open by the sink, its records have not yet been superseded by a rotation,
// and on a quiet install its mtime looks ancient precisely because nothing has
// been audited lately — the case where deleting it would be worst.
func TestReapAuditLogRemovesAgedGenerationsAndNeverTheLiveFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "audit.log")
	writeLogFileAged(t, live, 5000*24*time.Hour) // ancient by mtime, still the live file
	aged := writeAuditGeneration(t, live, 400)
	recent := writeAuditGeneration(t, live, 2)

	reapAuditLog(live, 30, testLogger())

	mustNotExist(t, aged, "a generation 400d past a 30d window must be reaped")
	mustExist(t, recent, "a generation inside the window must survive")
	mustExist(t, live, "the LIVE audit file must never be reaped, however old its mtime looks")
}

// TestReapAuditLogZeroDaysOrEmptyPathIsNoop pins the two off switches. A fresh
// upgrade leaves AuditLogDays unset and an install with no audit stream leaves
// AuditLogPath empty; if either meant "cutoff = now" or "sweep the CWD", the
// first nightly sweep after upgrading would destroy the archive it was added to
// protect. It must also not panic — it runs inside the nightly sweep.
func TestReapAuditLogZeroDaysOrEmptyPathIsNoop(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reapAuditLog panicked: %v", r)
		}
	}()
	dir := t.TempDir()
	live := filepath.Join(dir, "audit.log")
	writeLogFileAged(t, live, time.Hour)
	ancient := writeAuditGeneration(t, live, 5000)

	for _, days := range []int{0, -1} {
		reapAuditLog(live, days, testLogger())
		mustExist(t, ancient, "AuditLogDays="+strconv.Itoa(days)+" must keep every generation forever")
	}

	// An empty path must reap nothing, anywhere — not default to "." and sweep
	// the process working directory.
	reapAuditLog("", 1, testLogger())
	mustExist(t, ancient, "an empty AuditLogPath must disable the reaper entirely")
	mustExist(t, live, "an empty AuditLogPath must not touch the live file either")
}

// TestReapAuditLogOnlyMatchesEightDigitGenerations is the over-matching guard,
// and it carries more weight than the equivalent run-log test. The audit stream
// shares a directory with the process log and the run-log tree by default, and
// operators put their own files beside it — a hand-taken `audit.log.backup`
// before an upgrade is the obvious one. Prefix-only matching would delete all of
// them, and because they are compliance evidence nobody discovers the loss until
// they are asked to produce it.
func TestReapAuditLogOnlyMatchesEightDigitGenerations(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "audit.log")
	writeLogFileAged(t, live, time.Hour)

	survivors := []string{
		filepath.Join(dir, "audit.log.backup"),      // an operator's pre-upgrade copy
		filepath.Join(dir, "audit.log.2026"),        // four digits, not a generation
		filepath.Join(dir, "audit.log.202601011"),   // nine digits
		filepath.Join(dir, "audit.log.2026010"),     // seven digits
		filepath.Join(dir, "audit.log.abcdefgh"),    // eight characters, not digits
		filepath.Join(dir, "audit.log.20260101.gz"), // a generation someone compressed
		filepath.Join(dir, "audit.logger.20260101"), // a different base name entirely
	}
	for _, p := range survivors {
		writeLogFileAged(t, p, 5000*24*time.Hour)
	}
	victim := writeAuditGeneration(t, live, 400)

	reapAuditLog(live, 30, testLogger())

	for _, p := range survivors {
		mustExist(t, p, "only `<base>.<8 digits>` may be reaped — everything else is somebody's evidence")
	}
	mustNotExist(t, victim, "a genuine aged generation should still be reaped")
}

// TestAuditRetentionIsIndependentOfLogFilesDays is the property the whole
// separate-knob design exists for, exercised through runSweep so the wiring is
// covered too.
//
// Run logs age out in weeks; the audit stream must not. Three things have to
// hold at once and each is a different mechanism: a dated generation does not
// end in ".log" so the run-log reaper skips it by construction; the live
// audit.log DOES end in ".log" and survives only because it is in KeepFiles; and
// the audit reaper applies its own, longer window. Break any one and the 90-day
// run-log window quietly eats the compliance archive years early.
func TestAuditRetentionIsIndependentOfLogFilesDays(t *testing.T) {
	pool := migratedPool(t)
	logDir := t.TempDir()
	live := filepath.Join(logDir, "audit.log")
	runLog := filepath.Join(logDir, "aged-run.log")
	writeLogFileAged(t, live, 400*24*time.Hour) // ancient mtime, still live
	writeLogFileAged(t, runLog, 10*24*time.Hour)
	generation := writeAuditGeneration(t, live, 300)

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		LogDir:       logDir,
		LogFilesDays: 1, // run logs: aggressive
		KeepFiles:    []string{"audit.log"},
		AuditLogPath: live,
		AuditLogDays: 3650, // audit stream: years
		BackupDir:    t.TempDir(),
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	mustNotExist(t, runLog, "LogFilesDays=1 must still reap an aged run log")
	mustExist(t, generation, "a 300d-old audit generation must survive LogFilesDays=1 — the audit window is its own knob")
	mustExist(t, live, "the live audit.log must survive the run-log reaper via KeepFiles")
}

// TestSweepPassesAuditPolicyThrough is the plumbing half, in the other
// direction: a unit test on reapAuditLog alone stays green even if
// RetentionPolicy.AuditLogPath / .AuditLogDays are never read by the sweep, in
// which case the archive would grow without bound on every real install while
// the suite reported success.
func TestSweepPassesAuditPolicyThrough(t *testing.T) {
	pool := migratedPool(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "audit.log")
	writeLogFileAged(t, live, time.Hour)
	aged := writeAuditGeneration(t, live, 400)
	fresh := writeAuditGeneration(t, live, 1)

	if err := runSweep(context.Background(), pool, RetentionPolicy{
		AuditLogPath: live,
		AuditLogDays: 30,
		BackupDir:    t.TempDir(),
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	mustNotExist(t, aged, "RetentionPolicy.AuditLogDays must reach the reaper through runSweep")
	mustExist(t, fresh, "a generation inside the window must survive the sweep")
	mustExist(t, live, "the sweep must never remove the live audit file")
}

// FX-E1 — the sighting ledger ages out on the runs window, driven through the
// REAL sweep. (The first version of this test ran the DELETE itself, which
// proves the SQL works and nothing about the sweep — the exact
// reconstructed-post-state trap FX-D documented.)
func TestRetentionSweepsFileSightings(t *testing.T) {
	pool := migratedPool(t)
	// Distinct paths: the de-dupe UNIQUE index is over (job, path, size, mtime),
	// which is the whole point of the ledger — one row per file version.
	seed := func(id, seenAt string) {
		t.Helper()
		if _, err := pool.Exec(`
			INSERT INTO file_watch_sightings (id, job_source, job_name, path, size_bytes, mtime, seen_at)
			VALUES (?, 'git', 'ingest', ?, 1, '2020-01-01T00:00:00Z', ?)`,
			id, "/srv/in/"+id+".csv", seenAt); err != nil {
			t.Fatalf("seed sighting: %v", err)
		}
	}
	seed("old", "2020-01-01T00:00:00Z")
	seed("new", "2999-01-01T00:00:00Z")

	if err := runSweep(context.Background(), pool,
		RetentionPolicy{RunsDays: 30, BackupDir: t.TempDir()}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var left int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM file_watch_sightings`).Scan(&left)
	if left != 1 {
		t.Errorf("sightings after sweep = %d, want 1 (the aged row pruned, the future row kept) — "+
			"before FX-E1 this table was not swept at all and grew without bound", left)
	}
}

// SL-4 (the s3-logging plan) — while the S3 archive tier is on, the
// local reaper must not outrun the archive sweep. Pins the four answers of
// runAwaitsArchive as the reaper consumes them: a terminal run that has not been
// archived KEEPS its aged file; an archived one, a missing/failed-stamped one,
// and a file with no runs row at all reap as before. With the tier off, the
// reaper is byte-for-byte the pre-SL-4 one.
func TestReapLogFilesKeepsUnarchivedWhileArchiveTierOn(t *testing.T) {
	pool := migratedPool(t)
	logDir := t.TempDir()
	const (
		pending  = "0199a000-0000-7000-8000-00000000f001"
		archived = "0199a000-0000-7000-8000-00000000f002"
		missing  = "0199a000-0000-7000-8000-00000000f003"
		running  = "0199a000-0000-7000-8000-00000000f004"
	)
	seed := func(id, status string, archivedAt, state any) {
		if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, log_archived_at, log_archive_state)
			VALUES (?, 'j', 'bash', ?, 'tester', 'manual', ?, ?, ?)`, id, status, nowUTC().Format(time.RFC3339), archivedAt, state); err != nil {
			t.Fatal(err)
		}
	}
	seed(pending, "success", nil, nil)
	seed(archived, "success", "2026-09-01T00:00:00Z", nil)
	seed(missing, "failure", nil, "missing")
	seed(running, "running", nil, nil)
	files := map[string]string{}
	for _, id := range []string{pending, archived, missing, running, "orphan-no-row"} {
		files[id] = filepath.Join(logDir, id+".log")
		writeLogFileAged(t, files[id], 40*24*time.Hour)
	}
	ctx := context.Background()

	// Tier on: only the unarchived terminal run survives.
	keep := func(traceID string) bool { return runAwaitsArchive(ctx, pool, traceID) }
	reapLogFiles(logDir, 30, nil, keep, testLogger())
	mustExist(t, files[pending], "an unarchived terminal run's log must survive the window while the archive tier is on")
	mustNotExist(t, files[archived], "an archived run's local log reaps on the window")
	mustNotExist(t, files[missing], "a run stamped missing/failed left the pending set and reaps")
	mustNotExist(t, files[running], "a non-terminal run is not awaiting archive (its file is never this old in practice)")
	mustNotExist(t, files["orphan-no-row"], "a file with no runs row is the orphan case and reaps as before")

	// Tier off (nil hook): the survivor goes too.
	reapLogFiles(logDir, 30, nil, nil, testLogger())
	mustNotExist(t, files[pending], "with the archive tier off the pre-SL-4 reaper applies")
}

// TestSweepHonoursArchiveTierOn wires the same rule through runSweep's policy
// flag, the path main's Reload closure drives.
func TestSweepHonoursArchiveTierOn(t *testing.T) {
	pool := migratedPool(t)
	logDir := t.TempDir()
	const pending = "0199a000-0000-7000-8000-00000000f011"
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
		VALUES (?, 'j', 'bash', 'success', 'tester', 'manual', ?)`, pending, nowUTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(logDir, pending+".log")
	writeLogFileAged(t, f, 40*24*time.Hour)
	if err := runSweep(context.Background(), pool, RetentionPolicy{
		LogDir: logDir, LogFilesDays: 7, BackupDir: t.TempDir(), ArchiveTierOn: true,
	}, testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	mustExist(t, f, "ArchiveTierOn must keep an unarchived terminal run's log")
}
