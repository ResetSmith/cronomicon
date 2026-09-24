// Package logsync is the scheduled sweep that copies sealed run logs to the S3
// archive tier (SL-2, the s3-logging plan).
//
// The whole design is one query: terminal runs with no archive marker, oldest
// first (runs.log_archived_at / log_archive_state, migration 1130). That makes
// a tick idempotent and restart-safe, and the loop self-catching-up — a tick
// that was skipped, crashed, or fell behind simply finds more rows next time.
// Nothing is lost by a missed tick: the local file stays until the marker is
// set, and (SL-4) the local reaper deletes only archived files.
//
// Timing rules, each load-bearing:
//   - Single-flight (SL-Q10): a tick that fires while one is running is
//     SKIPPED, not queued, and the interval is measured from the END of the
//     last tick. That is what makes every interval safe for correctness; the
//     remaining failure modes are cost and backlog, not overlap.
//   - Budgeted (SL-Q7): a batch cap and a time budget per tick, leftovers
//     carry. First enable makes every log on disk pending; without the budget
//     that tick would hold the lock for hours and make "Sync now" look broken.
//   - Grace (SL-Q12): a run is eligible 60s after completed_at, past the
//     runner's last straggler chunk and the live-log finalize.
//   - Verify-by-size (SL-Q11): the marker is stamped only when the service's
//     reported size equals the local size read BEFORE the upload began, in the
//     same transaction as the counter bump.
//
// This package imports settings (the schedule reader) and runner (LogPath), so
// it sits above logarchive, which stays a leaf; nothing but main and the api
// mount import it.
package logsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Defaults for the tick budget and gates. Exposed as Options fields so tests
// can shrink them; production uses these.
const (
	DefaultBatchCap     = 200
	DefaultTickBudget   = 10 * time.Minute
	DefaultFileDeadline = 5 * time.Minute
	DefaultGrace        = 60 * time.Second
	// persistentFailureTicks is how many consecutive failed ticks it takes to
	// log at Error (the PP-M4 precedent: the backup's "alert" is a metric plus a
	// loud log line for the operator's log pipeline — notify's rules are keyed
	// by job-owned triggers and have no system trigger).
	persistentFailureTicks = 3
)

// terminalStatuses are the runs.status values a sealed log can belong to.
// `skipped` is deliberately absent: those rows never had a process and have
// no file; the tick stamps them `missing` up front so they leave the query.
const terminalStatuses = `'success','failure','warning','killed'`

// ErrInProgress is returned by RunOnce when a tick already holds the lock —
// what Sync now reports as 409.
var ErrInProgress = errors.New("a log archive sync is already running")

// ErrDisabled is returned by RunOnce when the backend is local (no store).
var ErrDisabled = errors.New("the S3 archive backend is not enabled")

// Options wires a Sweeper.
type Options struct {
	DB  *sql.DB
	Log *slog.Logger
	// Store returns the CURRENT archive store, nil while the backend is local.
	// Called per tick, never cached: the operator can re-save settings and the
	// api rebuilds the store (Server.applyLogArchive).
	Store func() *logarchive.Store
	// LogDir returns the effective run-log directory (settings.ResolveLogDir),
	// resolved per tick so an LU-5 repoint is followed without being told.
	LogDir func(ctx context.Context) string
	// WG, when set, tracks the loop goroutine for graceful shutdown.
	WG *sync.WaitGroup

	BatchCap     int
	TickBudget   time.Duration
	FileDeadline time.Duration
	Grace        time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// Sweeper runs the archive sync loop and serves Sync now.
type Sweeper struct {
	o Options

	mu         sync.Mutex // the single-flight lock; TryLock only
	inProgress atomic.Bool
	kick       chan struct{}
	// timerFired distinguishes a timer wake from a kick in loop; owned by the
	// loop goroutine only.
	timerFired bool

	// consecutiveFailures counts ticks in a row that ended with an error, for
	// the persistent-failure log line. Reset by a clean tick.
	consecutiveFailures int
}

// Result is what one tick did.
type Result struct {
	Uploaded  int
	Missing   int // local file gone before archive → stamped missing
	Failed    int // permanent S3 refusal → stamped failed (aborts the tick)
	Transient int // left pending for the next tick
	Skipped   int // `skipped` runs stamped missing up front
	Reconcile int // markers restored from objects already in the bucket
	Expired   int // archived objects past the archivedLogFiles window, deleted (SL-4)
	Pending   int64
	Aborted   bool
	Err       error
}

// New builds a Sweeper. Start runs the loop; RunOnce serves Sync now.
func New(o Options) *Sweeper {
	if o.BatchCap <= 0 {
		o.BatchCap = DefaultBatchCap
	}
	if o.TickBudget <= 0 {
		o.TickBudget = DefaultTickBudget
	}
	if o.FileDeadline <= 0 {
		o.FileDeadline = DefaultFileDeadline
	}
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Sweeper{o: o, kick: make(chan struct{}, 1)}
}

// SetStore binds the store getter after construction (main builds the Sweeper
// before the api Server that owns the store). Must be called before Start.
func (s *Sweeper) SetStore(fn func() *logarchive.Store) { s.o.Store = fn }

// Sync is the api-facing form of RunOnce (api.LogArchiveSyncer): result
// dropped, error kept.
func (s *Sweeper) Sync(ctx context.Context, reconcile bool, actor string) error {
	_, err := s.RunOnce(ctx, reconcile, actor)
	return err
}

// InProgress reports whether a tick holds the lock right now.
func (s *Sweeper) InProgress() bool { return s.inProgress.Load() }

// Kick wakes the loop so it re-reads the schedule (called after a settings
// save). Non-blocking; a pending kick coalesces.
func (s *Sweeper) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Start runs the loop until ctx is done. It returns immediately.
//
// While the backend is local the loop sleeps on Kick — no polling of an idle
// feature. Otherwise it arms: interval mode → the interval measured from the
// end of the last tick (so a tick that used its whole budget does not fire
// again at once); daily → the next UTC wall-clock occurrence. Boot catch-up:
// a last-finished older than one interval (24h daily) runs at once, mirroring
// the backup's overdue gate.
func (s *Sweeper) Start(ctx context.Context) {
	if s.o.WG != nil {
		s.o.WG.Add(1)
	}
	go func() {
		if s.o.WG != nil {
			defer s.o.WG.Done()
		}
		s.loop(ctx)
	}()
}

func (s *Sweeper) loop(ctx context.Context) {
	for {
		sch, err := settings.ReadLogSyncSchedule(ctx, s.o.DB)
		if err != nil {
			s.o.Log.Error("log archive: could not read the sync schedule; retrying in a minute", "error", err)
			if !s.wait(ctx, time.Minute) {
				return
			}
			continue
		}
		if !sch.Enabled {
			// Idle until settings change. -1 = no timer.
			if !s.wait(ctx, -1) {
				return
			}
			continue
		}
		delay := s.nextDelay(sch)
		if !s.wait(ctx, delay) {
			return
		}
		// A kick (settings change) also lands here; the top of the loop re-reads
		// the schedule and decides afresh. A timer expiry runs the tick.
		if s.timerFired {
			s.timerFired = false
			if _, err := s.RunOnce(ctx, false, "system"); err != nil {
				if !errors.Is(err, ErrInProgress) && !errors.Is(err, ErrDisabled) {
					s.o.Log.Warn("log archive: sync tick failed", "error", err)
				}
				// A tick that did not run (Sync now holds the lock, the store went
				// away, the finish stamp failed) leaves last_sync_finished_at where
				// it was, so nextDelay would answer 0 again at once. Back off so the
				// loop cannot spin; a kick still cuts the wait short.
				if !s.wait(ctx, 5*time.Second) {
					return
				}
			}
		}
	}
}

// nextDelay computes how long until the next tick under sch, measured from the
// end of the last tick. Zero means "now" (never run, or overdue).
func (s *Sweeper) nextDelay(sch settings.LogSyncSchedule) time.Duration {
	now := s.o.Now()
	switch sch.Mode {
	case "daily":
		if sch.LastFinishedAt.IsZero() || now.Sub(sch.LastFinishedAt) > 24*time.Hour {
			return 0
		}
		return cronutil.UntilWallClock(sch.At, "02:00", now)
	default:
		interval := time.Duration(sch.IntervalSeconds) * time.Second
		if sch.LastFinishedAt.IsZero() {
			return 0
		}
		elapsed := now.Sub(sch.LastFinishedAt)
		if elapsed >= interval {
			return 0
		}
		return interval - elapsed
	}
}

// wait blocks for d (no timer when d < 0) or until a kick or ctx. It returns
// false when ctx is done. timerFired records which one woke it.
func (s *Sweeper) wait(ctx context.Context, d time.Duration) bool {
	s.timerFired = false
	var timer <-chan time.Time
	if d >= 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.kick:
		return true
	case <-timer:
		s.timerFired = true
		return true
	}
}

// RunOnce performs one tick under the single-flight lock. reconcile prefixes
// the tick with a bucket listing that restores markers for objects already
// present (SL-Q13). actor names who asked — "system" for the timer, the
// session identity for Sync now — and goes on the activity row.
func (s *Sweeper) RunOnce(ctx context.Context, reconcile bool, actor string) (Result, error) {
	if !s.mu.TryLock() {
		return Result{}, ErrInProgress
	}
	defer s.mu.Unlock()
	s.inProgress.Store(true)
	defer s.inProgress.Store(false)

	if s.o.Store == nil {
		return Result{}, ErrDisabled
	}
	store := s.o.Store()
	if store == nil {
		return Result{}, ErrDisabled
	}
	res := s.tick(ctx, store, reconcile, actor)
	return res, res.Err
}

func (s *Sweeper) tick(ctx context.Context, store *logarchive.Store, reconcile bool, actor string) Result {
	var res Result
	db := s.o.DB
	log := s.o.Log
	started := s.o.Now()
	_, _ = db.ExecContext(ctx, `UPDATE log_storage_config SET last_sync_started_at=? WHERE id=1`, started.Format(time.RFC3339))
	logDir := s.o.LogDir(ctx)
	deadline := started.Add(s.o.TickBudget)

	// `skipped` runs never had a file; take them out of the pending set once.
	if r, err := db.ExecContext(ctx, `UPDATE runs SET log_archive_state='missing'
		WHERE status='skipped' AND log_archived_at IS NULL AND log_archive_state IS NULL`); err == nil {
		if n, _ := r.RowsAffected(); n > 0 {
			res.Skipped = int(n)
		}
	}

	if reconcile {
		n, err := s.reconcile(ctx, store)
		res.Reconcile = n
		if err != nil {
			res.Err = err
			s.finish(ctx, &res, actor)
			return res
		}
	}

	cutoff := started.Add(-s.o.Grace).Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(entity_code,'') FROM runs
		WHERE log_archived_at IS NULL AND log_archive_state IS NULL
		  AND status IN (`+terminalStatuses+`)
		  AND completed_at IS NOT NULL AND completed_at < ?
		ORDER BY completed_at ASC LIMIT ?`, cutoff, s.o.BatchCap)
	if err != nil {
		res.Err = fmt.Errorf("select pending runs: %w", err)
		s.finish(ctx, &res, actor)
		return res
	}
	type pending struct{ id, code string }
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.code); err == nil {
			batch = append(batch, p)
		}
	}
	rows.Close()

	metaSeen := map[string]bool{} // entity codes whose _meta.json is confirmed this tick
	var firstErr error
	for _, p := range batch {
		if ctx.Err() != nil {
			res.Err = ctx.Err()
			break
		}
		if s.o.Now().After(deadline) {
			log.Info("log archive: tick budget spent; remaining runs carry to the next tick", "uploaded", res.Uploaded)
			break
		}
		localPath, perr := runner.LogPath(logDir, p.code, p.id)
		if perr != nil {
			// A row the registry would never have produced; treat as missing so it
			// stops being selected, and say so.
			log.Warn("log archive: run has an unusable log path; marking missing", "trace_id", p.id, "entity_code", p.code, "error", perr)
			s.stampState(ctx, p.id, "missing")
			res.Missing++
			continue
		}
		info, serr := os.Stat(localPath)
		if serr != nil {
			if errors.Is(serr, os.ErrNotExist) {
				s.stampState(ctx, p.id, "missing")
				res.Missing++
				continue
			}
			log.Warn("log archive: cannot stat log file; leaving pending", "path", localPath, "error", serr)
			res.Transient++
			continue
		}
		localSize := info.Size()

		// The folder's sidecar names the folder's owner (SL-Q9). Once per code
		// per tick, best-effort: its absence must not block the log.
		if p.code != "" && !metaSeen[p.code] {
			s.ensureMeta(ctx, store, logDir, p.code)
			metaSeen[p.code] = true
		}

		key := store.Key(p.code, p.id)
		fctx, cancel := context.WithTimeout(ctx, s.o.FileDeadline)
		size, uerr := store.Put(fctx, key, localPath, "")
		cancel()
		if uerr != nil {
			if logarchive.IsPermanent(uerr) {
				// Every remaining row would fail the same way; stamp this one and
				// stop (SL-Q6).
				log.Error("log archive: upload refused; aborting tick", "trace_id", p.id, "key", key, "error", uerr)
				s.stampState(ctx, p.id, "failed")
				res.Failed++
				res.Aborted = true
				firstErr = uerr
				break
			}
			log.Warn("log archive: upload failed; will retry next tick", "trace_id", p.id, "key", key, "error", uerr)
			res.Transient++
			if firstErr == nil {
				firstErr = uerr
			}
			continue
		}
		if size != localSize {
			// The object exists but is not what we read; do not stamp. Next tick
			// re-uploads (Put overwrites).
			log.Warn("log archive: uploaded size differs from local; will retry", "trace_id", p.id, "local", localSize, "remote", size)
			res.Transient++
			if firstErr == nil {
				firstErr = fmt.Errorf("size mismatch for %s: local %d, remote %d", key, localSize, size)
			}
			continue
		}
		if err := s.stampArchived(ctx, p.id, size, s.o.Now()); err != nil {
			log.Error("log archive: uploaded but could not record it; will re-upload next tick", "trace_id", p.id, "error", err)
			res.Transient++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res.Uploaded++
	}
	if res.Err == nil {
		res.Err = firstErr
	}

	// SL-4: the archive tier's own retention window, applied by the sweep
	// rather than the nightly reaper because this is the process that holds
	// the store. Skipped after an aborted tick — a bucket refusing PUTs will
	// refuse DELETEs, and the marker must not be cleared on a failed delete.
	if !res.Aborted && res.Err == nil {
		n, eerr := s.expireArchived(ctx, store, started)
		res.Expired = n
		if eerr != nil {
			res.Err = eerr
		}
	}
	s.finish(ctx, &res, actor)
	return res
}

// expireArchived deletes archived objects older than the archivedLogFiles knob
// (0 = never) and clears their markers, in batches of BatchCap per tick. The
// marker is cleared only after the delete succeeded, in the same transaction as
// the counter decrement; a delete refused with AccessDenied stops the pass for
// this tick and is reported once — the knob's help text says why (the bucket
// policy withholds delete; use a lifecycle rule instead).
//
// A folder's _meta.json sidecar is removed when its last log goes, so the
// bucket does not accumulate empty folders that name a job nothing is left of.
func (s *Sweeper) expireArchived(ctx context.Context, store *logarchive.Store, now time.Time) (int, error) {
	ac, err := settings.GetAuditCompliance(ctx, s.o.DB)
	if err != nil {
		return 0, fmt.Errorf("expire: read retention: %w", err)
	}
	days := ac.RetentionDays.ArchivedLogFiles
	if days <= 0 {
		return 0, nil
	}
	cutoff := now.AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, COALESCE(entity_code,'') FROM runs
		WHERE log_archived_at IS NOT NULL AND log_archived_at < ? ORDER BY log_archived_at ASC LIMIT ?`, cutoff, s.o.BatchCap)
	if err != nil {
		return 0, fmt.Errorf("expire: select: %w", err)
	}
	type expired struct{ id, code string }
	var batch []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.code); err == nil {
			batch = append(batch, e)
		}
	}
	rows.Close()

	deleted := 0
	codes := map[string]bool{}
	for _, e := range batch {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		key := store.Key(e.code, e.id)
		size, _, herr := store.Head(ctx, key)
		if herr != nil {
			if logarchive.IsPermanent(herr) {
				return deleted, fmt.Errorf("expire: %w", herr)
			}
			continue // transient; next tick
		}
		if derr := store.Delete(ctx, key); derr != nil {
			if logarchive.IsPermanent(derr) {
				s.o.Log.Warn("log archive: expiry refused by the bucket — grant s3:DeleteObject or expire with a lifecycle rule and set the window to 0",
					"key", key, "error", derr)
				return deleted, fmt.Errorf("expire: %w", derr)
			}
			continue
		}
		if err := s.clearArchived(ctx, e.id, size); err != nil {
			s.o.Log.Error("log archive: object deleted but marker not cleared; reconcile will not restore it", "trace_id", e.id, "error", err)
			continue
		}
		deleted++
		if e.code != "" {
			codes[e.code] = true
		}
	}
	// Sidecars for folders that are now empty.
	for code := range codes {
		objs, lerr := store.List(ctx, code+"/")
		if lerr != nil {
			continue
		}
		if len(objs) == 1 && objs[0].Key == store.MetaKey(code) {
			_ = store.Delete(ctx, objs[0].Key)
		}
	}
	return deleted, nil
}

// clearArchived reverses stampArchived and stamps `expired` (SL-4). The state
// matters: a bare NULL would put the run back in the pending set, and while its
// local file outlives the archive window the next tick would re-upload what
// this one just deleted. `expired` leaves the pending set like missing/failed
// do, lets the local reaper reap the file on its own window, and reads as
// "no log" (the reader only consults the marker).
func (s *Sweeper) clearArchived(ctx context.Context, traceID string, size int64) error {
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET log_archived_at=NULL, log_archive_state='expired' WHERE id=?`, traceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE log_storage_config
		SET archived_count=MAX(archived_count-1, 0), archived_bytes=MAX(archived_bytes-?, 0) WHERE id=1`, size); err != nil {
		return err
	}
	return tx.Commit()
}

// stampArchived records a verified upload and bumps the counters atomically.
func (s *Sweeper) stampArchived(ctx context.Context, traceID string, size int64, at time.Time) error {
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET log_archived_at=?, log_archive_state=NULL WHERE id=?`, at.Format(time.RFC3339), traceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE log_storage_config SET archived_count=archived_count+1, archived_bytes=archived_bytes+? WHERE id=1`, size); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Sweeper) stampState(ctx context.Context, traceID, state string) {
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE runs SET log_archive_state=? WHERE id=? AND log_archived_at IS NULL`, state, traceID); err != nil {
		s.o.Log.Error("log archive: could not stamp archive state", "trace_id", traceID, "state", state, "error", err)
	}
}

// ensureMeta uploads {logDir}/{code}/_meta.json if the bucket lacks it and the
// file exists locally. Best-effort throughout.
func (s *Sweeper) ensureMeta(ctx context.Context, store *logarchive.Store, logDir, code string) {
	key := store.MetaKey(code)
	if _, found, err := store.Head(ctx, key); err != nil || found {
		return
	}
	local := filepath.Join(logDir, code, runner.MetaFileName)
	if _, err := os.Stat(local); err != nil {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, s.o.FileDeadline)
	defer cancel()
	if _, err := store.Put(fctx, key, local, "application/json"); err != nil {
		s.o.Log.Debug("log archive: could not upload folder sidecar", "key", key, "error", err)
	}
}

// reconcile lists the bucket under the prefix and restores markers for runs
// whose object is already present (SL-Q13): the down-then-up migration case,
// or a DB restored from a snapshot older than the last sync. It also resets
// the counters to what the listing says, which is the honest repair for any
// crash between an upload and its stamp.
func (s *Sweeper) reconcile(ctx context.Context, store *logarchive.Store) (int, error) {
	objs, err := store.List(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("reconcile: %w", err)
	}
	var count, bytes int64
	restored := 0
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".log") {
			continue
		}
		count++
		bytes += o.Size
		traceID := strings.TrimSuffix(o.Key[strings.LastIndex(o.Key, "/")+1:], ".log")
		if !runner.ValidTraceID(traceID) {
			continue
		}
		r, uerr := s.o.DB.ExecContext(ctx, `UPDATE runs SET log_archived_at=?, log_archive_state=NULL
			WHERE id=? AND log_archived_at IS NULL`, o.LastModified.UTC().Format(time.RFC3339), traceID)
		if uerr != nil {
			return restored, fmt.Errorf("reconcile: stamp %s: %w", traceID, uerr)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			restored++
		}
	}
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE log_storage_config SET archived_count=?, archived_bytes=? WHERE id=1`, count, bytes); err != nil {
		return restored, fmt.Errorf("reconcile: counters: %w", err)
	}
	return restored, nil
}

// finish stamps the tick's end and error, updates the metrics, writes the
// activity row when the tick did something, and handles the persistent-failure
// log line.
func (s *Sweeper) finish(ctx context.Context, res *Result, actor string) {
	db := s.o.DB
	now := s.o.Now()
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE log_archived_at IS NULL AND log_archive_state IS NULL
		AND status IN (`+terminalStatuses+`)`).Scan(&res.Pending)
	metrics.LogArchivePending(res.Pending)

	var errText *string
	if res.Err != nil {
		t := res.Err.Error()
		errText = &t
		metrics.LogArchiveFailed()
		s.consecutiveFailures++
		if s.consecutiveFailures == persistentFailureTicks {
			s.o.Log.Error("log archive: sync has failed on consecutive ticks; run logs are accumulating unarchived",
				"consecutive", s.consecutiveFailures, "pending", res.Pending, "error", res.Err)
		}
	} else {
		s.consecutiveFailures = 0
		metrics.LogArchiveSucceeded(now)
	}
	_, _ = db.ExecContext(ctx, `UPDATE log_storage_config SET last_sync_finished_at=?, last_sync_error=? WHERE id=1`,
		now.Format(time.RFC3339), errText)

	// One activity row per tick that did something; an idle tick writes nothing,
	// or a 5-minute interval floods the feed.
	if res.Uploaded+res.Failed+res.Transient+res.Missing+res.Reconcile+res.Expired > 0 {
		outcome := "success"
		if res.Failed > 0 || res.Err != nil {
			outcome = "failure"
		} else if res.Transient > 0 || res.Missing > 0 {
			outcome = "warning"
		}
		summary := fmt.Sprintf("Log archive sync: %d uploaded", res.Uploaded)
		var extra []string
		if res.Reconcile > 0 {
			extra = append(extra, fmt.Sprintf("%d reconciled", res.Reconcile))
		}
		if res.Expired > 0 {
			extra = append(extra, fmt.Sprintf("%d expired", res.Expired))
		}
		if res.Missing > 0 {
			extra = append(extra, fmt.Sprintf("%d missing locally", res.Missing))
		}
		if res.Transient > 0 {
			extra = append(extra, fmt.Sprintf("%d retrying", res.Transient))
		}
		if res.Failed > 0 {
			extra = append(extra, fmt.Sprintf("%d refused", res.Failed))
		}
		if len(extra) > 0 {
			summary += " (" + strings.Join(extra, ", ") + ")"
		}
		details := ""
		if res.Err != nil {
			details = res.Err.Error()
		}
		if err := auditlog.WriteActivity(ctx, db, auditlog.ActivityParams{
			At: now.Format(time.RFC3339), Kind: "config", Outcome: outcome, Actor: actor,
			Target: "log-archive", Category: "Settings", Action: "sync", Summary: summary, Details: details,
		}); err != nil {
			s.o.Log.Warn("log archive: could not write activity row", "error", err)
		}
	}
	s.o.Log.Info("log archive: tick finished",
		"uploaded", res.Uploaded, "missing", res.Missing, "transient", res.Transient, "failed", res.Failed,
		"reconciled", res.Reconcile, "expired", res.Expired, "pending", res.Pending, "aborted", res.Aborted, "error", res.Err)
}
