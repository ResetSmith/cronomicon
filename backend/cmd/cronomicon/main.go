// Command cronomicon is the single static backend binary (T1/T3): it serves the
// API + embedded frontend, and exposes a `validate` subcommand (T11) for
// CI-time YAML linting that reuses the runtime parser.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	// time/tzdata embeds the IANA zone database in the single static binary
	// (OD-TZ1) so time.LoadLocation("America/...") works in slim containers that
	// lack /usr/share/zoneinfo. Without it the app timezone feature would silently
	// degrade to UTC/time.Local.
	_ "time/tzdata"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auditsink"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/backup"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/logsink"
	"github.com/ResetSmith/cronomicon/internal/logsync"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/redactdict"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/seed"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/workflow"
	"github.com/ResetSmith/cronomicon/web"
)

// errIdentityNotReady fails /readyz when OIDC is configured but the relying
// party hasn't come up (T12/T13).
var errIdentityNotReady = errors.New("identity provider not ready")

// Build metadata, stamped via -ldflags at build time (Dockerfile B.1):
//
//	-X main.version=... -X main.commit=... -X main.buildDate=...
//
// Surfaced at /version and /healthz so a deployed image is identifiable.
// `version` is the fallback for un-stamped builds (e.g. `go build`, `make` without
// tags); a release build still overrides all three via -ldflags. Kept in sync with
// the top CHANGELOG.md entry.
var (
	version   = "1.5.45"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	// Subcommand dispatch.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "validate":
			// `cronomicon validate <path>` lints YAML job definitions (T11).
			os.Exit(runValidate(os.Args[2:]))
		case "healthcheck":
			// `cronomicon healthcheck` is the container HEALTHCHECK probe: the
			// distroless image has no shell/curl, so the binary probes itself
			// over HTTP (B.5). Exit 0 = ready, non-zero = unhealthy.
			os.Exit(runHealthcheck(os.Args[2:]))
		case "restore":
			// `cronomicon restore` fetches a snapshot from the S3 backup bucket and
			// swaps it into place, then runs PRAGMA integrity_check (FU-3 Phase C).
			// Stop the server first. See backend/deploy/backup-restore.md.
			os.Exit(runRestore(os.Args[2:]))
		case "rewrap-secrets":
			// `cronomicon rewrap-secrets` re-wraps every stored credential under the
			// active KEK so a superseded key can actually be retired (DR-5).
			// Rotation is otherwise lazy: a row moves only when it is rewritten.
			// Runs ONLINE — no need to stop the server.
			os.Exit(runRewrapSecrets(os.Args[2:]))
		case "grant-admin":
			// `cronomicon grant-admin <ad-group|email>` is the break-glass admin
			// lockout recovery (RF-25/RB-Q15): on OIDC deployments the
			// CRONOMICON_BOOTSTRAP_ADMIN_GROUP floor does not apply, so this is the
			// only supported way back in. Stop the server first.
			os.Exit(runGrantAdmin(os.Args[2:]))
		case "version":
			fmt.Printf("cronomicon %s (commit %s, built %s)\n", version, commit, buildDate)
			os.Exit(0)
		}
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, logSink := newLogger(cfg)
	slog.SetDefault(logger)
	// The file destination is attached below, once the DB can tell us where the
	// log directory is. An explicit CRONOMICON_LOG_FILE needs no lookup, so attach
	// it now and capture the banner and config warnings too.
	if cfg.LogFileEnabled && cfg.LogFilePath != "" {
		if err := logSink.SetPath(cfg.LogFilePath); err != nil {
			logger.Error("process log file unavailable; continuing on stdout only", "path", cfg.LogFilePath, "error", err)
		}
	}
	defer logSink.Close()

	// LU-10: the compliance audit stream. Constructed pathless for the same
	// reason as the process log — the run-log directory lives in the database —
	// and attached below. Installing the sink into auditlog now rather than after
	// the attach is deliberate: a disabled sink drops writes silently, so nothing
	// is lost, and it means no audit event emitted during startup can slip past
	// because the wiring had not happened yet.
	auditSink := auditsink.New(auditsink.Options{Report: func(err error) {
		// This CAN log through slog — audit records do not feed the process
		// logger, so there is no recursion — and it MUST: an audit stream that
		// stops silently is worse than one never enabled, because the resulting
		// gap is indistinguishable from a period with no events.
		logger.Error("audit stream write failed; the database remains authoritative", "error", err)
	}})
	defer auditSink.Close()
	auditlog.SetSink(auditSink.Write)

	logger.Info("cronomicon build", "version", version, "commit", commit, "built", buildDate)

	// DR-6: refuse a world-readable KEK file HERE, at boot, rather than letting
	// the refusal surface hours later on the first secret operation. Nothing
	// loads the KEK until something needs it, so without this the check would
	// fire at an arbitrary moment. Group-readable warns from inside the same
	// call. A no-op when the KEK comes from the environment.
	if err := secrets.VerifyKEKFileMode(cfg); err != nil {
		return err
	}

	// Process-lifetime context for background workers (retention, long-poll
	// dispatch later). Cancelled on shutdown signal below.
	ctx0, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// bg tracks ctx0-bound background workers (scheduler, SSH executor loop +
	// in-flight runs, runner reaper, retention) so graceful shutdown can drain
	// them before pool.Close() runs — no query races a closing pool (PP-L15).
	var bg sync.WaitGroup

	// Database: open, apply migrations on boot (T4), expose readiness (T13).
	pool, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		return fmt.Errorf("migrate db: %w", err)
	}
	schemaVersion, _, _ := db.Status(pool)
	logger.Info("database ready", "path", cfg.DBPath, "schema_version", schemaVersion)

	// AM-5: the audit-stream masker. Wired here — after migrate, before the
	// first audit write (role refresh, seed, retention all come later) — so no
	// change_log / activity / auth_events row is stored before the dictionary
	// exists. The store rebuilds itself on every secret / SSH-key / encrypted-
	// setting / env_var write (secrets.RedactionSourceChanged) and reports a
	// degraded build once per outage as an `Audit / redactor-unavailable` row.
	redactdict.Install(pool, cfg, logger)

	// RB-7: load the role→permission matrix into its process cache. requirePerm is
	// mount-time middleware with no context and no DB handle, so authorization reads
	// this cache rather than the table; it is refreshed on every role write (each of
	// which also revokes all other sessions). A failure here is NOT fatal — the
	// registry keeps the compiled-in built-ins, which is exactly how the server
	// behaved before v0.56.1 — but it is loud, because it means custom roles are
	// silently inert.
	if err := auth.RefreshRoles(ctx0, pool); err != nil {
		logger.Error("could not load the roles table; falling back to the built-in permission matrix — custom roles will NOT apply",
			"error", err)
	}

	// LU-4: attach cronomicon.log now that the run-log directory is readable. Only
	// the build banner and config warnings precede this point, so nothing of
	// diagnostic value is stdout-only — but the attach line repeats the build
	// stamp so the file is self-identifying without cross-referencing stdout.
	if p := processLogPath(cfg, settings.ResolveLogDir(ctx0, pool)); p != "" {
		if err := logSink.SetPath(p); err != nil {
			logger.Error("process log file unavailable; continuing on stdout only", "path", p, "error", err)
		} else {
			logger.Info("process log file attached",
				"path", p, "max_mb", cfg.LogFileMaxMB, "keep", cfg.LogFileKeep,
				"version", version, "commit", commit)
		}
	}

	if p := auditLogPath(cfg, settings.ResolveLogDir(ctx0, pool)); p != "" {
		if err := auditSink.SetPath(p); err != nil {
			logger.Error("audit stream unavailable; audit rows are still written to the database", "path", p, "error", err)
		} else {
			logger.Info("audit stream attached", "path", p)
		}
	}

	// Local-preview demo data (CRONOMICON_DEV_SEED). No-op unless enabled and the DB
	// is empty, so it never touches a real (GitLab-synced) database.
	if cfg.DevSeed {
		if err := seed.Seed(ctx0, pool, logger); err != nil {
			logger.Error("demo seed failed", "error", err)
		}
	}

	// Identity: OIDC relying party + operator sessions (B2). Never
	// hard-fails — login degrades if the issuer is unreachable (T12).
	authSvc := auth.NewService(ctx0, cfg, pool, logger)

	// Readiness checks are registered by subsystems as they're wired. DB is
	// first; dependency probes (identity provider, GitLab) attach in their stages.
	readyChecks := []api.ReadyCheck{
		{Name: "database", Check: db.ReadyCheck(pool)},
	}
	// The identity provider is the one hard-required dependency (T12): when OIDC is
	// configured, /readyz must fail until the relying party is live.
	if cfg.OIDC.Enabled() {
		readyChecks = append(readyChecks, api.ReadyCheck{
			Name: "identity",
			Check: func(context.Context) error {
				if !authSvc.Enabled() {
					return errIdentityNotReady
				}
				return nil
			},
		})
	}

	// Nightly retention sweep + backup snapshot + S3 upload (A4).
	uploader, backupEnabled, err := backup.New(cfg, logger)
	if err != nil {
		logger.Error("backup uploader init failed; nightly S3 upload disabled", "error", err)
	} else if backupEnabled {
		logger.Info("backup S3 upload enabled", "bucket", cfg.BackupS3Bucket)
	}
	// LU-2 (closes G2): the Audit & Compliance settings blob is authoritative for
	// retention; the CRONOMICON_RETENTION_* env vars are the bootstrap default only.
	// Seed the blob from env once, on the first boot that finds it unset, so an
	// existing deployment that set CRONOMICON_RETENTION_RUNS_DAYS=30 keeps 30 across
	// this upgrade instead of silently reverting to the blob's untouched 90.
	// Non-fatal: on failure the reload below falls back to the env-derived policy.
	if seeded, serr := settings.SeedAuditComplianceFromEnv(ctx0, pool,
		cfg.RetentionRunsDays, cfg.RetentionChangeLogDays, cfg.RetentionLogFilesDays); serr != nil {
		logger.Error("could not seed retention settings from env; the stored blob's defaults apply", "error", serr)
	} else if seeded {
		logger.Info("retention settings seeded from environment",
			"runs_days", cfg.RetentionRunsDays,
			"change_log_days", cfg.RetentionChangeLogDays,
			"log_files_days", cfg.RetentionLogFilesDays)
	}
	db.StartRetention(ctx0, pool, db.RetentionPolicy{
		// Boot values, used only if the very first Reload fails.
		RunsDays:           cfg.RetentionRunsDays,
		ActivityDays:       cfg.RetentionRunsDays,
		WorkflowRunsDays:   cfg.RetentionRunsDays,
		ChangeLogDays:      cfg.RetentionChangeLogDays,
		SchedulePushesDays: cfg.RetentionChangeLogDays,
		LogFilesDays:       cfg.RetentionLogFilesDays,
		// LU-4 lives in this tree and ends in .log like every run log, but the
		// process holds it open — see RetentionPolicy.KeepFiles.
		// Both live files sit in this tree and end in .log like every run log, but
		// each is held open and rotated on its own schedule — see
		// RetentionPolicy.KeepFiles. Their dated/numbered generations do NOT end
		// in .log, so the run-log reaper skips those by construction.
		KeepFiles: []string{processLogName, auditLogName},
		// RH: the reaper and the manual "Purge now" button share one implementation
		// of what permanent deletion means. Injected because internal/db must not
		// import internal/api.
		PurgeDefinition: func(ctx context.Context, kind, name string) (bool, error) {
			return api.PurgeDefinition(ctx, pool, settings.ResolveLogDir(ctx, pool), kind, name)
		},
		BackupAt: cfg.BackupAt, // PP-H6: wall-clock daily sweep
		Upload:   uploader,     // nil when unconfigured ⇒ snapshot kept locally only
		WG:       &bg,          // PP-L15: drained on shutdown before pool.Close()
		// Re-read the authoritative knobs at the top of every sweep so a settings
		// change takes effect on the next nightly run rather than the next restart
		// (LU-2). The closure lives here, not in internal/db, so that package keeps
		// no dependency on internal/settings.
		Reload: func(ctx context.Context, p db.RetentionPolicy) db.RetentionPolicy {
			ac, err := settings.GetAuditCompliance(ctx, pool)
			if err != nil {
				// Keep the last-known-good windows: returning zeroes would read as
				// "keep forever" and silently stop retention.
				logger.Error("retention: could not read settings; using previous policy", "error", err)
				return p
			}
			p.RunsDays = ac.RetentionDays.Runs
			p.ActivityDays = ac.RetentionDays.Activity
			p.WorkflowRunsDays = ac.RetentionDays.WorkflowRuns
			p.ChangeLogDays = ac.RetentionDays.ChangeLog
			p.SchedulePushesDays = ac.RetentionDays.SchedulePushes
			p.LogFilesDays = ac.RetentionDays.LogFiles
			p.AuditLogDays = ac.RetentionDays.AuditLogFiles
			p.RecycleBinDays = ac.RetentionDays.RecycleBin
			p.DefinitionRevisionsDays = ac.RetentionDays.DefinitionRevisions
			p.RunnerPlacementHistoryDays = ac.RetentionDays.RunnerPlacementHistory
			// Resolved per sweep too: the operator can re-point the log dir, and
			// the reaper must sweep the tree that is actually being written to.
			p.LogDir = settings.ResolveLogDir(ctx, pool)
			p.AuditLogPath = auditLogPath(cfg, p.LogDir)
			// SL-4: while the S3 archive tier is on, the local reaper must not
			// outrun the archive sweep. A read failure leaves the previous value.
			if sch, serr := settings.ReadLogSyncSchedule(ctx, pool); serr == nil {
				p.ArchiveTierOn = sch.Enabled
			}
			return p
		},
	}, logger)

	// Stale-runner reaper (V1.1-8.4 / R3): sweep last_seen_at, offline crashed
	// runners + reconcile their orphaned runs to terminal, and deregister runners
	// offline past the deregister window. Mirrors StartRetention's goroutine
	// lifecycle. Wired with the notifier so runner_lost failures fire the same
	// metrics + notifications as every other terminal path.
	notifier := notify.New(pool, cfg, logger).WithShutdownWG(&bg) // PP-L15: drain notify dispatch
	runner.New(pool, cfg, logger).
		WithNotifier(notifier).
		WithShutdownWG(&bg). // PP-L15
		StartReaper(ctx0)

	// Cron scheduler (B5): load enabled scheduled jobs/workflows and fire them,
	// enqueuing runs that runners drain. The instance is retained (not discarded)
	// so the git-sync hook can call ReloadIfChanged when new schedules land.
	// Start() runs cron then blocks on ctx for graceful stop, so it lives in its
	// own goroutine and unwinds when ctx0 is cancelled.
	// Resolve the effective application timezone (the app setting if set+loadable,
	// else time.Local — which Go derives from $TZ) so the cron engine fires in the
	// configured zone, not just the container's TZ (timezone-update §3/§4). Logged
	// for operator visibility.
	gs, gserr := settings.GetGlobalSettings(ctx0, pool)
	if gserr != nil {
		logger.Warn("could not load settings for timezone resolution; falling back to host zone", "err", gserr)
	}
	appLoc := settings.ResolveEffectiveTimezone(gs)
	logger.Info("scheduler timezone resolved", "zone", appLoc.String())
	sched := scheduler.New(pool, logger, appLoc)
	// Workflow firer: scheduled workflows can't be dispatched from the scheduler
	// package (it must not import workflow), so the scheduler calls back here. We
	// look up the workflow's current steps and start a run tagged scheduled, with
	// the firing schedule's name and env snapshot.
	// SL — the scheduler's own alert sink, shared with the runner's so SLA
	// breaches and missed fires reach the same transports, rules and
	// per-transport delivery stamps as a terminal-run notification.
	sched.SetNotifier(notifier)
	sched.SetWorkflowFirer(func(ctx context.Context, source, workflowName, scheduleName, envJSON string) {
		var (
			rowid     int64
			stepsJSON string
		)
		// Source-qualified lookup (A9/A11) so a scheduled cronomicon workflow resolves
		// its own steps rather than a same-named git workflow's.
		if err := pool.QueryRowContext(ctx,
			`SELECT rowid, steps FROM workflows WHERE source = ? AND name = ?`, source, workflowName,
		).Scan(&rowid, &stepsJSON); err != nil {
			logger.Error("scheduler firer: look up workflow", "workflow", workflowName, "source", source, "error", err)
			return
		}
		steps, err := workflow.ParseSteps(stepsJSON)
		if err != nil {
			logger.Error("scheduler firer: parse steps", "workflow", workflowName, "error", err)
			return
		}
		if _, err := workflow.New(pool, logger).Trigger(ctx, workflow.TriggerParams{
			WorkflowName:   workflowName,
			WorkflowSource: source,
			WorkflowID:     rowid,
			Steps:          steps,
			TriggeredBy:    "scheduler",
			TriggerKind:    "scheduled",
			ScheduleName:   scheduleName,
			EnvJSON:        envJSON,
		}); err != nil {
			logger.Error("scheduler firer: trigger workflow", "workflow", workflowName, "error", err)
		}
	})
	// AR — deferred ad-hoc WORKFLOW runs promoted by the pending loop fire
	// through the same engine seam as the cron firer, but keep the scheduling
	// operator as triggered_by so the run's audit trail names a person.
	sched.SetPendingWorkflowFirer(func(ctx context.Context, p scheduler.PendingWorkflowFire) {
		var (
			rowid     int64
			stepsJSON string
		)
		if err := pool.QueryRowContext(ctx,
			`SELECT rowid, steps FROM workflows WHERE source = ? AND name = ?`, p.Source, p.Name,
		).Scan(&rowid, &stepsJSON); err != nil {
			logger.Error("pending firer: look up workflow", "workflow", p.Name, "source", p.Source, "error", err)
			return
		}
		steps, err := workflow.ParseSteps(stepsJSON)
		if err != nil {
			logger.Error("pending firer: parse steps", "workflow", p.Name, "error", err)
			return
		}
		// RX-11 — the trigger kind and the reaction provenance ride the envelope
		// rather than being hardcoded here. A reaction-fired workflow that
		// reported "manual" would be a lie in History, and a depth that reset to
		// zero on every hop would disarm the runaway ceiling for whole chains.
		if _, err := workflow.New(pool, logger).Trigger(ctx, workflow.TriggerParams{
			WorkflowName:   p.Name,
			WorkflowSource: p.Source,
			WorkflowID:     rowid,
			Steps:          steps,
			TriggeredBy:    p.TriggeredBy,
			TriggerKind:    p.TriggerKind,
			EnvJSON:        p.EnvJSON,
			ReactionDepth:  p.ReactionDepth,
			ReactedToRunID: p.ReactedToRunID,
		}); err != nil {
			logger.Error("pending firer: trigger workflow", "workflow", p.Name, "error", err)
		}
	})
	bg.Go(func() {
		if err := sched.Start(ctx0); err != nil {
			logger.Error("scheduler stopped with error", "error", err)
		}
	})

	// SL-2: the S3 log-archive sweep. Constructed before the api so its Kick can
	// be the server's LogArchiveChanged hook; its store getter is bound to the
	// server right after api.New and before Start.
	archiveSync := logsync.New(logsync.Options{
		DB:     pool,
		Log:    logger,
		LogDir: func(ctx context.Context) string { return settings.ResolveLogDir(ctx, pool) },
		WG:     &bg,
	})

	srv := api.New(api.Options{
		Config:         cfg,
		Logger:         logger,
		Auth:           authSvc,
		DB:             pool,
		ReadyChecks:    readyChecks,
		WebFS:          web.DistFS(),
		Build:          api.BuildInfo{Version: version, Commit: commit, Date: buildDate},
		Context:        ctx0, // PP-M6: bind the SSH executor to the process lifetime
		ShutdownWG:     &bg,  // PP-L15: drain SSH executor + in-flight runs on shutdown
		ScheduleReload: sched.ReloadIfChanged,
		// Unconditional reload for in-app (cronomicon-source) definition writes, which
		// don't advance the git SHA the onSyncComplete hook gates on (A9 / v20 Phase 3).
		ScheduleForceReload: func(ctx context.Context) { _ = sched.Reload(ctx) },
		// Rebuild the cron engine in a new zone when the operator changes the
		// timezone setting (robfig/cron can't re-zone in place) (timezone-update §4.2).
		ScheduleTimezoneReload: sched.RebuildWithLocation,
		LogArchiveSync:         archiveSync,
		LogArchiveChanged:      archiveSync.Kick,
		// LU-5: the API re-points its own run-log writers; the process log is the
		// one consumer it can't reach, because main owns the sink. A no-op when
		// the path is pinned via CRONOMICON_LOG_FILE or file logging is off —
		// processLogPath returns the same value, and SetPath ignores a re-set.
		LogDirChanged: func(_ context.Context, dir string) {
			p := processLogPath(cfg, dir)
			if err := logSink.SetPath(p); err != nil {
				logger.Error("could not move the process log to the new log directory; continuing on stdout only",
					"path", p, "error", err)
				return
			}
			if p != "" {
				logger.Info("process log file re-pointed", "path", p)
			}
			ap := auditLogPath(cfg, dir)
			if err := auditSink.SetPath(ap); err != nil {
				logger.Error("could not move the audit stream to the new log directory", "path", ap, "error", err)
				return
			}
			if ap != "" {
				logger.Info("audit stream re-pointed", "path", ap)
			}
		},
	})

	archiveSync.SetStore(srv.LogArchive)
	archiveSync.Start(ctx0)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No global WriteTimeout: runner log-ingest (T6) and long-poll (A6.2)
		// are intentionally long-lived; those handlers manage their own deadlines.
		IdleTimeout: 5 * time.Minute, // reclaim idle keep-alive connections (PP-M1)
	}

	// Graceful shutdown on SIGINT/SIGTERM (ctx0 above carries the same signal).
	errCh := make(chan error, 1)
	go func() {
		logger.Info("cronomicon starting", "addr", cfg.Addr, "db", cfg.DBPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	case <-ctx0.Done():
		logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		// PP-L15: wait for ctx0-bound background workers (scheduler, SSH executor
		// loop + in-flight runs, runner reaper) to unwind before the deferred
		// pool.Close() runs, so no in-flight query races a closing pool. Bounded
		// by the same drain budget — if a worker overruns we proceed anyway.
		waitWithTimeout(shutdownCtx, &bg, logger)
		logger.Info("shutdown complete")
		return nil
	}
}

// waitWithTimeout blocks until wg drains or ctx expires (the shutdown budget).
func waitWithTimeout(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		logger.Warn("background workers did not drain within the shutdown budget")
	}
}

// newLogger builds the one process logger, over a logsink.Writer so the file
// destination can be attached and re-pointed later without rebuilding it
// (LU-4). It returns the sink so run() can do exactly that once the DB is open.
//
// The sink starts with no file: the run-log directory lives in the database,
// which is not open yet. Only the build banner and any config warnings are
// emitted before run() attaches the file, and those are replayed there so
// nothing that matters is stdout-only.
func newLogger(cfg *config.Config) (*slog.Logger, *logsink.Writer) {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	sink := logsink.New(os.Stdout, logsink.Options{
		MaxBytes: int64(cfg.LogFileMaxMB) << 20,
		Keep:     cfg.LogFileKeep,
	})
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(sink, opts)
	} else {
		h = slog.NewJSONHandler(sink, opts)
	}
	return slog.New(h), sink
}

// processLogPath returns where cronomicon.log should live: the explicit
// CRONOMICON_LOG_FILE when set, else cronomicon.log beside the run logs. Empty means
// file logging is switched off.
func processLogPath(cfg *config.Config, logDir string) string {
	if !cfg.LogFileEnabled {
		return ""
	}
	if cfg.LogFilePath != "" {
		return cfg.LogFilePath
	}
	return filepath.Join(logDir, processLogName)
}

// auditLogPath returns where audit.log should live: the explicit
// CRONOMICON_AUDIT_LOG when set, else audit.log beside the run logs. Empty means
// the audit stream is switched off and only the database records events.
func auditLogPath(cfg *config.Config, logDir string) string {
	if !cfg.AuditLogEnabled {
		return ""
	}
	if cfg.AuditLogPath != "" {
		return cfg.AuditLogPath
	}
	return filepath.Join(logDir, auditLogName)
}

// auditLogName is excluded from the run-log reaper alongside processLogName, and
// is the prefix its own reaper matches dated generations against.
const auditLogName = "audit.log"

// processLogName is also what main excludes from the LU-1 reaper: the reaper
// collects *.log by mtime, and removing this one out from under an open handle
// would send every later line to an unlinked inode.
const processLogName = "cronomicon.log"

// runValidate implements `cronomicon validate <path>...` (T11), reusing the runtime
// YAML/pragma parser from the gitlab slice (T10/S10). A path that is a directory
// is validated as a whole job-definitions checkout: scripts/ is parsed first and
// each job's script_ref is resolved against it (B-Git), with orphan scripts
// reported as non-fatal warnings. A file path is validated standalone as before.
// Exit 0 = valid, 1 = errors, 2 = usage.
func runValidate(args []string) int {
	return validatePaths(args, os.Stdout, os.Stderr)
}

// validatePaths is the testable core of `cronomicon validate` (V1.1-1 / Q2): it
// writes the "ok"/error report to the given writers and returns the process exit
// code (0 = valid, 1 = errors, 2 = usage) without calling os.Exit, so a CLI-level
// test can assert both the exit code and the line-numbered stderr format against
// a deliberately-broken repo.
func validatePaths(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(errOut, "usage: cronomicon validate <path>... (file, or a repo dir for cross-file script_ref checks)")
		return 2
	}
	exit := 0
	for _, path := range args {
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			errs, warnings, err := gitlab.ValidateRepo(path)
			if err != nil {
				fmt.Fprintf(errOut, "%s: %v\n", path, err)
				exit = 1
				continue
			}
			for _, w := range warnings {
				fmt.Fprintf(errOut, "warning: %s\n", w.Error())
			}
			for _, e := range errs {
				fmt.Fprintln(errOut, e.Error())
				exit = 1
			}
			if len(errs) == 0 {
				fmt.Fprintf(out, "%s: ok\n", path)
			}
			continue
		}
		errs, err := gitlab.ValidateFile(path)
		if err != nil {
			fmt.Fprintf(errOut, "%s: %v\n", path, err)
			exit = 1
			continue
		}
		for _, e := range errs {
			fmt.Fprintf(errOut, "%s:%d: %s\n", path, e.Line, e.Message)
			exit = 1
		}
		if len(errs) == 0 {
			fmt.Fprintf(out, "%s: ok\n", path)
		}
	}
	return exit
}
