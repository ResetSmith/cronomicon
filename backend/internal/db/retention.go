package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/metrics"
)

// backupInterval is the minimum spacing between backups: the boot catch-up only
// fires when the last success is older than this, so a crash-loop redeploying
// many times a day still backs up at most ~once (PP-H6).
const backupInterval = 24 * time.Hour

// defaultBackupAt is the wall-clock UTC time the daily sweep targets when
// RetentionPolicy.BackupAt is unset.
const defaultBackupAt = "02:00"

// RetentionPolicy holds the per-table day knobs (A4). Defaults: 90d for
// execution data, 1yr for audit data. Zero means "keep forever".
type RetentionPolicy struct {
	// One knob per pruned table (LU-2). These used to be two fields covering
	// five tables, which meant three of the six knobs in the Audit & Compliance
	// settings blob had no field to map onto and were therefore inert (G2).
	RunsDays           int // runs
	ActivityDays       int // activity
	WorkflowRunsDays   int // workflow_runs
	ChangeLogDays      int // change_log
	SchedulePushesDays int // schedule_pushes
	// LogFilesDays bounds on-disk run logs (LU-1, closes G1). Until this landed
	// nothing reaped {LogDir}/*.log — including files whose owning runs row had
	// already been pruned — so the log volume grew without limit. Zero means
	// "keep forever", matching the day knobs above.
	LogFilesDays int
	// RecycleBinDays bounds how long a soft-deleted cronomicon-source definition
	// stays restorable (RH). Zero means keep forever, like every knob here — but
	// note the asymmetry: for every OTHER knob "forever" costs disk, while for
	// this one it costs a name, because a binned definition still occupies its
	// (source, name) primary key and blocks a replacement from being created.
	RecycleBinDays int
	// DefinitionRevisionsDays bounds the append-only snapshot history (RH).
	DefinitionRevisionsDays int
	// RunnerPlacementHistoryDays bounds the DR-7 placement snapshots. The
	// default and its reasoning live on the settings knob (DR-Q6).
	RunnerPlacementHistoryDays int
	// PurgeDefinition hard-deletes one binned definition, wired to the API's
	// purgeDefinition so the reaper and the manual "Purge now" button cannot
	// disagree about what permanent deletion means (trigger cascades, the entity
	// code retiring, the log folder being annotated rather than destroyed).
	// Injected rather than reimplemented here because internal/db must not import
	// internal/api; nil disables the definition purge entirely.
	PurgeDefinition func(ctx context.Context, kind, name string) (bool, error)
	// LogDir is the effective run-log directory (settings.ResolveLogDir). Empty
	// disables the reaper entirely, so a lookup failure degrades to today's
	// behaviour rather than sweeping the wrong tree.
	LogDir string
	// ArchiveTierOn reports that the S3 log-archive backend is enabled (SL-4).
	// While it is, the local reaper never removes a run log the archive sweep
	// has not yet copied — otherwise a `logFiles` window shorter than an outage
	// of the bucket deletes logs that were never archived, and the tier that
	// exists for disaster recovery loses exactly the logs it was meant to keep.
	// Files with no runs row (the orphan case the reaper was built for) and
	// runs already archived or stamped missing/failed reap as before. Refreshed
	// by Reload like every other knob.
	ArchiveTierOn bool
	// KeepFiles are base names under LogDir the reaper must never remove.
	// The process log (LU-4) lives in this tree and ends in .log like every run
	// log, but it is held open for the process's whole life and rotated on its
	// own keep-count. Reaping it would not free the space — the handle keeps the
	// inode alive — and every subsequent line would be written to a file no
	// longer reachable by name.
	KeepFiles []string
	// AuditLogPath is the live audit stream file (LU-10); its dated generations
	// are `<path>.YYYYMMDD` siblings. Empty disables the audit reaper.
	AuditLogPath string
	// AuditLogDays is the audit stream's own retention window — deliberately
	// separate from, and longer than, LogFilesDays.
	//
	// The two must not share a knob. A dated generation is named
	// `audit.log.20260727`, which does NOT end in ".log", so the run-log reaper
	// above skips it by construction; the live `audit.log` does end in ".log" and
	// is therefore protected by KeepFiles instead. Without both of those, the
	// 90-day run-log window would quietly delete the compliance stream long
	// before its own window expired — the exact failure the longer window exists
	// to prevent.
	AuditLogDays int
	BackupDir    string
	// BackupAt is the daily wall-clock UTC time ("HH:MM") for the sweep. Empty ⇒
	// defaultBackupAt. Wall-clock anchoring (vs a 24h-from-boot ticker) means a
	// process that restarts more often than daily can't keep pushing the only
	// durable backup past 24h (PP-H6).
	BackupAt string
	// Upload, if non-nil, ships each nightly snapshot to S3 (A4). main wires it
	// from internal/backup so this package needs no S3/settings dependency.
	Upload func(ctx context.Context, localPath string) error
	// Reload, if non-nil, is called at the top of every sweep to refresh the
	// day knobs and LogDir from their authoritative source (LU-2/LU-Q3(a): the
	// DB settings blob, with env as the bootstrap default). It takes and returns
	// the policy by value so this package stays a leaf — main supplies the
	// closure over internal/settings.
	//
	// Without it the policy is captured once at boot and a retention change
	// needs a restart to take effect, which is exactly the kind of surprise the
	// settings panel would otherwise create. On error the closure is expected to
	// return the policy it was given, so a transient DB hiccup falls back to the
	// last-known-good knobs rather than to zero (= keep forever).
	Reload func(ctx context.Context, p RetentionPolicy) RetentionPolicy
	// WG, when set, tracks the sweep goroutine so graceful shutdown can drain it
	// before the pool closes (PP-L15).
	WG *sync.WaitGroup
}

// StartRetention launches the daily sweep in a goroutine (A4): prune aged rows
// per policy, then VACUUM INTO a snapshot for the S3 backup. It returns
// immediately; the worker stops when ctx is cancelled.
//
// Scheduling (PP-H6): a boot catch-up sweep runs immediately when the last
// successful backup is overdue, then the worker re-arms a timer for the next
// configured wall-clock time each day — so backups no longer depend on the
// process surviving a full 24h.
func StartRetention(ctx context.Context, pool *sql.DB, p RetentionPolicy, log *slog.Logger) {
	if p.WG != nil {
		p.WG.Add(1)
	}
	go func() {
		if p.WG != nil {
			defer p.WG.Done()
		}
		// Boot catch-up: back up now iff overdue (gated on persisted last-success
		// so a crash-loop doesn't back up on every restart).
		if backupOverdue(ctx, pool, backupInterval) {
			if err := runSweep(ctx, pool, p, log); err != nil {
				log.Error("retention initial sweep failed", "error", err)
			}
		}
		for {
			timer := time.NewTimer(untilNext(p.BackupAt, nowUTC()))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if err := runSweep(ctx, pool, p, log); err != nil {
					log.Error("retention sweep failed", "error", err)
				}
			}
		}
	}()
}

// untilNext returns the duration from now to the next occurrence of the "HH:MM"
// UTC wall-clock time (defaultBackupAt when hhmm is empty/invalid).
func untilNext(hhmm string, now time.Time) time.Duration {
	return cronutil.UntilWallClock(hhmm, defaultBackupAt, now)
}

// backupOverdue reports whether the last successful backup is missing or older
// than interval (PP-H6 boot-catch-up gate).
func backupOverdue(ctx context.Context, pool *sql.DB, interval time.Duration) bool {
	var v sql.NullString
	_ = pool.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='backupLastSuccessAt'`).Scan(&v)
	if !v.Valid || v.String == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return true
	}
	return nowUTC().Sub(t) >= interval
}

// persistLastSuccess records the time of a successful backup (PP-H6) in the
// flat-KV settings table — read back by backupOverdue across restarts.
func persistLastSuccess(ctx context.Context, pool *sql.DB, t time.Time) {
	_, _ = pool.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES('backupLastSuccessAt', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, t.Format(time.RFC3339))
}

func runSweep(ctx context.Context, pool *sql.DB, p RetentionPolicy, log *slog.Logger) (err error) {
	// PP-H6/PP-M4: a real sweep failure increments the failure counter so
	// monitoring can alert; success records the last-success gauge + timestamp
	// below. A clean-shutdown cancellation (ctx0 cancelled) is NOT a backup
	// failure — it legitimately produced no new backup and the next overdue boot
	// catch-up recovers it — so it must not trip the alert.
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			metrics.BackupFailed()
		}
	}()
	// LU-2: pick up settings changes without a restart. Done before the deferred
	// hook can matter and before any DELETE runs, so a sweep always prunes to the
	// currently-configured windows rather than the ones in force at boot.
	if p.Reload != nil {
		p = p.Reload(ctx, p)
	}
	prune := func(table, tsCol string, days int) error {
		if days <= 0 {
			return nil
		}
		cutoff := nowUTC().AddDate(0, 0, -days).Format(time.RFC3339)
		res, err := pool.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE %s < ?", table, tsCol), cutoff) //nolint:gosec // table/col are package constants
		if err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			log.Info("retention pruned rows", "table", table, "deleted", n, "cutoff", cutoff)
		}
		return nil
	}

	for _, t := range []struct {
		table, col string
		days       int
	}{
		{"runs", "created_at", p.RunsDays},
		{"workflow_runs", "created_at", p.WorkflowRunsDays},
		{"activity", "created_at", p.ActivityDays},
		{"change_log", "created_at", p.ChangeLogDays},
		// auth_events is change-log-class audit data and shares its window. It
		// deliberately has no knob of its own: a separate one would be a seventh
		// number in the panel answering the same compliance question, and the
		// audit.log stream — which carries these events too — already has the
		// longer, independent window (LU-Q4(c)).
		{"auth_events", "created_at", p.ChangeLogDays},
		{"schedule_pushes", "created_at", p.SchedulePushesDays},
		// RX — the reaction delivery log is a delivery log, not a permanent
		// record: History owns the permanent trail, and a fired reaction's run
		// carries reacted_to_run_id on the run row precisely so the because-of
		// link outlives this table. It shares the runs window because a delivery
		// only means anything alongside the run it produced — keeping deliveries
		// after their runs are gone would leave rows pointing at nothing.
		{"reaction_deliveries", "delivered_at", p.RunsDays},
		// FX-E1 — the file-arrival sighting ledger. It shares the runs window for
		// the reaction_deliveries reason: a sighting only means anything alongside
		// the run it did or did not produce. Before this entry the table was not
		// swept at all, so a busy watch grew it without bound.
		{"file_watch_sightings", "seen_at", p.RunsDays},
		// RH — the snapshot history. A plain age prune: a revision is a record,
		// not a live row, and nothing references it.
		{"definition_revisions", "created_at", p.DefinitionRevisionsDays},
		// DR-7 — placement snapshots for deregistered runners. Without this the
		// table grows without bound, slowly: a row per deregistration.
		{"runner_placement_history", "deregistered_at", p.RunnerPlacementHistoryDays},
		// FX-B4 — TERMINAL pending rows only (see the guarded prune below): a
		// 'missed' row is a record of a run that never happened, and it shares the
		// runs window because that is the trail it belongs to. Live 'pending' rows
		// are work, not history, and pruning one by age would silently cancel a run
		// an operator scheduled far ahead — so this cannot use the plain age prune
		// the rows above do, and did not appear here at all before, which let
		// missed rows accumulate forever.
	} {
		if err := prune(t.table, t.col, t.days); err != nil {
			return err
		}
	}

	// FX-B4 — the guarded half of the pending_runs prune. Status-scoped, so a
	// far-future scheduled run is never swept out from under the operator who
	// scheduled it.
	if p.RunsDays > 0 {
		cutoff := nowUTC().AddDate(0, 0, -p.RunsDays).Format(time.RFC3339)
		// created_at is the fallback bound: a row can be marked missed BECAUSE its
		// run_at failed to parse, and such a value compares lexically against an
		// RFC3339 cutoff — it may never sort below one, leaving the row forever.
		res, err := pool.ExecContext(ctx,
			`DELETE FROM pending_runs
			  WHERE status = 'missed' AND run_at < ? AND created_at < ?`, cutoff, cutoff)
		if err != nil {
			return fmt.Errorf("prune pending_runs: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Info("retention pruned rows", "table", "pending_runs", "deleted", n, "cutoff", cutoff)
		}
	}

	// RH — hard-delete definitions whose recycle-bin window has closed.
	//
	// Deliberately NOT a row in the table above: a definition purge is not a
	// DELETE by timestamp. It must fire the delete-cascade triggers, retire the
	// entity code so a later definition of the same name gets a fresh log folder,
	// and annotate that folder rather than destroy it — which is exactly what the
	// API's purge route does, so the two share one implementation.
	if err := purgeExpiredDefinitions(ctx, pool, p, log); err != nil {
		return err
	}

	// Prune consumed action_queue entries older than 7 days (PP-L12). Entries
	// are marked consumed_at by drainControl; without pruning they accumulate
	// indefinitely and bloat the DB.
	if err := prune("action_queue", "consumed_at", 7); err != nil {
		return err
	}

	// LU-1: reap aged run-log files. Deliberately placed BEFORE the backup block
	// rather than beside the snapshot GC below: VACUUM INTO needs free space, so
	// a full disk fails the backup and would skip the one step that reclaims any.
	// reapLogFiles never returns an error — see its doc comment.
	var keepUnarchived func(traceID string) bool
	if p.ArchiveTierOn {
		keepUnarchived = func(traceID string) bool { return runAwaitsArchive(ctx, pool, traceID) }
	}
	reapLogFiles(p.LogDir, p.LogFilesDays, p.KeepFiles, keepUnarchived, log)
	// LU-10: the audit stream ages out on its own, longer window.
	reapAuditLog(p.AuditLogPath, p.AuditLogDays, log)

	// A4 nightly backup: VACUUM INTO a consistent snapshot, then upload to S3.
	dir := p.BackupDir
	if dir == "" {
		dir = "/var/lib/cronomicon/backups"
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create backup dir %s: %w", dir, err)
	}
	snapshot := filepath.Join(dir, fmt.Sprintf("cronomicon-%s.db", nowUTC().Format("20060102")))
	// PP-M2: SQLite's VACUUM INTO refuses to overwrite an existing file, so a
	// second sweep on the same UTC day (boot catch-up, clock change, manual run)
	// would error AFTER the prune already ran — pruning rows but producing no
	// backup. Remove any prior same-day snapshot first so the write is idempotent.
	if err := os.Remove(snapshot); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale snapshot %s: %w", snapshot, err)
	}
	if _, err := pool.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return fmt.Errorf("vacuum into %s: %w", snapshot, err)
	}
	log.Info("retention snapshot written", "path", snapshot)

	// Ship it offsite (A4). Optional: skipped when no uploader is configured.
	// The KEK for stored secrets must NOT live in this same bucket (S14).
	if p.Upload != nil {
		if err := p.Upload(ctx, snapshot); err != nil {
			return fmt.Errorf("upload snapshot: %w", err)
		}
	}

	// Prune local snapshot files older than 7 days so they don't accumulate
	// on the volume indefinitely (PP-M5). Only removes *.db files matching
	// the cronomicon-YYYYMMDD.db pattern to avoid touching anything unexpected.
	if entries, err := os.ReadDir(dir); err == nil {
		cutoffFile := fmt.Sprintf("cronomicon-%s.db", nowUTC().AddDate(0, 0, -7).Format("20060102"))
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() && strings.HasPrefix(name, "cronomicon-") && strings.HasSuffix(name, ".db") && name < cutoffFile {
				if rerr := os.Remove(filepath.Join(dir, name)); rerr != nil {
					log.Warn("retention: failed to remove old snapshot", "file", name, "error", rerr)
				} else {
					log.Info("retention: removed old snapshot", "file", name)
				}
			}
		}
	}

	// Success: record the durable artifact's time so monitoring can alert on a
	// stale/never-run backup, and the boot catch-up can gate on it (PP-H6).
	now := nowUTC()
	persistLastSuccess(ctx, pool, now)
	metrics.BackupSucceeded(now)
	return nil
}

// reapLogFiles removes run-log files under dir whose mtime predates the
// retention cutoff, then prunes the directories left empty (LU-1, closes G1).
//
// It never returns an error, deliberately. runSweep's deferred hook trips
// metrics.BackupFailed() on any non-context error it returns, so a transient
// log-dir problem — an unreadable mount, a file another process removed first —
// must not both abort the nightly backup and raise a false backup alarm. Every
// failure is logged and the sweep continues, mirroring the snapshot GC's error
// discipline below.
//
// Reaping is by mtime, not by joining against runs: the owning run row is very
// often pruned first (both are on RunsDays), and those orphans are precisely the
// files that need collecting.
//
// Only *.log files are candidates. The directory is operator-configurable
// (settings.ResolveLogDir), so the suffix filter is what stops a mistyped path
// from turning the reaper loose on unrelated files — the same reason the
// snapshot GC below matches cronomicon-*.db and nothing else. The walk is recursive
// so it already handles the per-entity subtree LU-7 introduces.
//
// keepUnarchived, when non-nil (SL-4: the archive tier is on), is asked per
// run-log file whether its run is still waiting to be archived; a yes keeps the
// file regardless of age. The file's base name minus ".log" is the trace id —
// the same two-shape layout runner.LogPath writes, so the flat and the foldered
// case both resolve.
func reapLogFiles(dir string, days int, keep []string, keepUnarchived func(traceID string) bool, log *slog.Logger) {
	if dir == "" || days <= 0 {
		return
	}
	cutoff := nowUTC().AddDate(0, 0, -days)
	var removed, failed, awaiting int
	// WalkDir stats with Lstat and does not follow symlinks, so a link planted in
	// the log dir cannot widen the blast radius to another tree.
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".log") {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		if slices.Contains(keep, d.Name()) {
			return nil // held open by another writer; see KeepFiles
		}
		info, ierr := d.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			return nil //nolint:nilerr // unstattable or still within retention
		}
		if keepUnarchived != nil && keepUnarchived(strings.TrimSuffix(d.Name(), ".log")) {
			awaiting++
			return nil
		}
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			log.Warn("retention: failed to remove aged log file", "file", path, "error", rerr)
			failed++
			return nil
		}
		removed++
		return nil
	}); err != nil && !os.IsNotExist(err) {
		// A missing log dir is the normal state of an install that has not run
		// anything yet — the writers create it lazily — so it is not worth a
		// nightly warning.
		log.Warn("retention: log dir walk failed", "dir", dir, "error", err)
	}
	if removed > 0 || failed > 0 || awaiting > 0 {
		log.Info("retention reaped log files",
			"dir", dir, "removed", removed, "failed", failed, "awaiting_archive", awaiting, "cutoff", cutoff.Format(time.RFC3339))
	}
	pruneEmptyLogDirs(dir, log)
}

// runAwaitsArchive reports whether the run behind a log file is still pending
// archive (SL-4): a runs row exists, it is terminal, and it carries neither the
// archive marker nor a missing/failed state. A run that is not terminal is
// writing its log and is never aged past the window in practice; a run with no
// row is an orphan and reaps as before. A query error answers false — the
// reaper's pre-SL-4 behaviour — and is logged by the caller's sweep summary
// only through the count, deliberately: a transient DB hiccup must not turn
// the nightly sweep into a no-op forever.
func runAwaitsArchive(ctx context.Context, pool *sql.DB, traceID string) bool {
	var status string
	var archived, stamped bool
	err := pool.QueryRowContext(ctx, `SELECT status, log_archived_at IS NOT NULL, log_archive_state IS NOT NULL
		FROM runs WHERE id = ?`, traceID).Scan(&status, &archived, &stamped)
	if err != nil {
		return false
	}
	switch status {
	case "success", "failure", "warning", "killed":
		return !archived && !stamped
	}
	return false
}

// reapAuditLog removes dated generations of the audit stream older than the
// cutoff (LU-10). The live file is never touched: it is held open, and its
// content has not yet been superseded by a rotation.
//
// Selection is by exact name prefix — `<basename>.` followed by eight digits —
// rather than by suffix, so nothing else in the directory can match. That
// matters more here than for run logs: the audit stream shares a directory with
// the process log and the run-log tree by default, and this is compliance
// evidence, so an over-broad match would delete precisely the thing an auditor
// came for.
//
// Like reapLogFiles it never returns an error: a failure here must not trip the
// backup-failed alarm or abort the nightly sweep.
func reapAuditLog(path string, days int, log *slog.Logger) {
	if path == "" || days <= 0 {
		return
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("retention: audit log dir unreadable", "dir", dir, "error", err)
		}
		return
	}
	cutoff := nowUTC().AddDate(0, 0, -days).Format("20060102")
	prefix := base + "."
	var removed int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		day := strings.TrimPrefix(name, prefix)
		if len(day) != 8 || !allDigits(day) {
			continue // not one of ours
		}
		// Lexical comparison is a date comparison for YYYYMMDD, the same trick the
		// snapshot GC uses.
		if day >= cutoff {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, name)); rerr != nil && !os.IsNotExist(rerr) {
			log.Warn("retention: failed to remove aged audit log", "file", name, "error", rerr)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Info("retention reaped audit logs", "dir", dir, "removed", removed, "cutoff", cutoff)
	}
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// pruneEmptyLogDirs removes directories left empty under root — never root
// itself, which must survive for the next run to write into.
//
// os.Remove refuses a non-empty directory, so it is its own emptiness check: a
// concurrent run that has just created its log file makes the remove fail
// harmlessly, where a separate stat-then-remove would race and delete it.
//
// A narrow window remains, and only opens once LU-7 introduces per-entity
// folders: a run that has done MkdirAll but not yet created its log file has an
// empty directory this would collect, failing that run's file create. Today the
// log dir is flat, so there is nothing here to walk. Revisit with LU-7 —
// creating the file before the sweep can see the bare directory, or skipping
// directories younger than the cutoff, both close it.
func pruneEmptyLogDirs(root string, log *slog.Logger) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Deepest first, so a parent whose only child was just removed is itself
	// collected in the same pass. A child's path is always strictly longer than
	// its parent's, so descending length is a valid child-before-parent order.
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
	for _, p := range dirs {
		if err := os.Remove(p); err == nil {
			log.Info("retention: removed empty log directory", "dir", p)
		}
	}
}

// nowUTC is a seam for tests.
var nowUTC = func() time.Time { return time.Now().UTC() }

// purgeExpiredDefinitions hard-deletes soft-deleted cronomicon-source definitions
// whose recycle-bin window has closed (RH).
//
// It reads the names first and purges them one at a time through the injected
// PurgeDefinition rather than issuing a bulk DELETE, because "purge" means more
// than removing a row: the delete-cascade triggers have to fire, the entity code
// has to retire so a later definition of the same name mints a fresh log folder,
// and that folder has to be annotated rather than destroyed. A bulk DELETE here
// would do the first of those and silently skip the rest.
//
// A single failure is logged and the sweep continues: one wedged definition must
// not stop the other two kinds from being reaped.
func purgeExpiredDefinitions(ctx context.Context, pool *sql.DB, p RetentionPolicy, log *slog.Logger) error {
	if p.RecycleBinDays <= 0 || p.PurgeDefinition == nil {
		return nil // keep forever, or no purger wired (tests)
	}
	cutoff := nowUTC().AddDate(0, 0, -p.RecycleBinDays).Format(time.RFC3339)
	type victim struct{ kind, name string }
	var victims []victim
	for kind, table := range map[string]string{
		"job":      "jobs",
		"workflow": "workflows",
		"schedule": "schedules",
	} {
		rows, err := pool.QueryContext(ctx,
			`SELECT name FROM `+table+`
			  WHERE source='cronomicon' AND deleted_at IS NOT NULL AND deleted_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("recycle-bin scan %s: %w", table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("recycle-bin scan %s: %w", table, err)
			}
			victims = append(victims, victim{kind, name})
		}
		rows.Close()
	}
	purged := 0
	for _, v := range victims {
		ok, err := p.PurgeDefinition(ctx, v.kind, v.name)
		if err != nil {
			log.Error("retention: purge definition", "kind", v.kind, "name", v.name, "err", err)
			continue
		}
		if ok {
			purged++
		}
	}
	if purged > 0 {
		log.Info("retention: purged expired definitions", "count", purged, "olderThanDays", p.RecycleBinDays)
	}
	return nil
}
