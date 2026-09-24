// Package api wires the HTTP surface. v1 uses the stdlib net/http ServeMux
// (Go 1.22+ method+pattern routing) per T1 — no third-party router.
//
// Generation direction is spec-first with oapi-codegen (T5, decided Jun 9):
// openapi.yaml is canonical and single-owner; generated types/interfaces are
// adopted as handlers are implemented (B3–B6). Until a slice is generated, its
// routes are hand-mounted here against the same contract.
package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
)

// Server owns the dependencies every handler needs. Feature packages (auth,
// scheduler, gitlab, runner) attach their handlers via the Mount* methods so
// B3–B6 can grow the surface without editing a single god-struct.
type Server struct {
	cfg         *config.Config
	log         *slog.Logger
	auth        *auth.Service
	db          *sql.DB // shared pool; feature mount files build their services from it
	readyChecks []ReadyCheck
	webFS       fs.FS // built frontend assets (web/dist), embedded
	build       BuildInfo
	// runCtx is the process-lifetime context (main.go's ctx0). Long-running
	// background workers mounted here — notably the SSH executor — bind to it so
	// they unwind on graceful shutdown (PP-M6). Defaults to context.Background()
	// when unset (tests), preserving the prior detached behavior.
	runCtx context.Context
	// shutdownWG, when set, lets those workers be drained before the pool closes
	// at shutdown (PP-L15).
	shutdownWG *sync.WaitGroup
	// sshProbe runs settings-page "Test connection" probes. It is a probe-only
	// sshexec.Service (no claim loop / Start), kept as one long-lived instance so
	// its per-target in-flight guard is shared across requests.
	sshProbe *sshexec.Service
	// scheduleReload, when set, is handed to the gitlab Service as its
	// onSyncComplete hook so a git sync triggers an immediate scheduler reload.
	// Set from Options (main.go wires it to scheduler.ReloadIfChanged).
	scheduleReload func(ctx context.Context, sha string)
	// scheduleForceReload unconditionally rebuilds the scheduler's cron entries.
	// Invoked after in-app (amadeus-source) definition writes, which don't advance
	// the git SHA the onSyncComplete hook gates on (A9 / v20 Phase 3).
	scheduleForceReload func(ctx context.Context)
	// scheduleTimezoneReload rebuilds the cron engine in a new effective app zone
	// (robfig/cron fixes its location at construction). Invoked after the operator
	// saves a changed timezone (timezone-update §4.2). Wired to
	// scheduler.RebuildWithLocation in main.go.
	scheduleTimezoneReload func(ctx context.Context, loc *time.Location) error
	// logDirChanged notifies main that the run-log directory moved (LU-5), so it
	// can re-point the process log when that file is derived from this path.
	logDirChanged func(ctx context.Context, dir string)
	// runnerSvc and sshExec are the two in-process run-log writers, retained
	// (rather than constructed-and-dropped in mountRunners) so a log-directory
	// change can be pushed to them without a restart (LU-5).
	runnerSvc *runner.Service
	sshExec   *sshexec.Service
	// logDir caches the effective run-log directory so the per-request writers
	// (writeSSHTestLog) read the same in-process value the long-lived services
	// hold, instead of re-querying the DB on their own schedule. One convention,
	// one source — before LU-5 the two disagreed after a settings change.
	logDir atomic.Pointer[string]
	// logArchive is the S3 archive tier's store (SL-1), nil while the backend is
	// local. Built from the stored settings at mount and rebuilt after every
	// successful log-storage save (applyLogArchive), so the sweep (SL-2) and the
	// reader fallback (SL-3) always load the client the operator most recently
	// saved — the same no-restart contract logDir has.
	logArchive atomic.Pointer[logarchive.Store]
	// logArchiveSync / logArchiveChanged — see Options.
	logArchiveSync    LogArchiveSyncer
	logArchiveChanged func()
	// appLoc caches the effective application timezone (settings.Timezone, else
	// time.Local) so schedule next-run/upcoming projections evaluate in the SAME
	// zone the cron engine fires in (timezone-update §4.4). Initialized at New from
	// the DB and refreshed in-process by handleUpdateGeneralSettings on a zone
	// change — avoiding a settings read per request. Read via appLocation().
	appLoc atomic.Pointer[time.Location]
}

// BuildInfo carries the version/commit/date stamped into the binary at build
// time (B.1), surfaced at /version and /healthz.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Options carries everything New needs to assemble the server.
type Options struct {
	Config      *config.Config
	Logger      *slog.Logger
	Auth        *auth.Service
	DB          *sql.DB
	ReadyChecks []ReadyCheck
	WebFS       fs.FS
	Build       BuildInfo
	// Context is the process-lifetime context for background workers started at
	// mount time (the SSH executor). Wired to main.go's ctx0 so a SIGTERM unwinds
	// them (PP-M6). Optional; nil ⇒ context.Background().
	Context context.Context
	// ShutdownWG, when set, tracks those background workers so they can be drained
	// before the DB pool closes on shutdown (PP-L15). Optional.
	ShutdownWG *sync.WaitGroup
	// ScheduleReload triggers an immediate scheduler reload after a git sync
	// (wired to scheduler.ReloadIfChanged in main.go). Optional.
	ScheduleReload func(ctx context.Context, sha string)
	// ScheduleForceReload unconditionally rebuilds the scheduler's cron entries,
	// invoked after in-app definition writes (wired to scheduler.Reload). Optional.
	ScheduleForceReload func(ctx context.Context)
	// ScheduleTimezoneReload rebuilds the cron engine in a new effective app zone
	// after the operator changes the timezone setting (wired to
	// scheduler.RebuildWithLocation in main.go). Optional.
	ScheduleTimezoneReload func(ctx context.Context, loc *time.Location) error
	// LogDirChanged is called after the operator saves a new run-log directory
	// (LU-5). The in-process consumers — the runner service and the SSH executor
	// — are re-pointed by the Server itself; this hook exists for the one
	// consumer it cannot reach, the process-log sink owned by main. Optional.
	LogDirChanged func(ctx context.Context, dir string)
	// LogArchiveSync is the archive sweep (internal/logsync), wired by main, for
	// "Sync now" and the in-progress flag (SL-2). Optional: nil disables the
	// endpoint with 503 and reports inProgress=false.
	LogArchiveSync LogArchiveSyncer
	// LogArchiveChanged is called after the archive store is rebuilt from a
	// settings save, so the sweep re-reads its schedule (SL-2). Optional.
	LogArchiveChanged func()
}

// LogArchiveSyncer is what the api needs from the archive sweep. Defined here
// so api does not import internal/logsync (main wires the concrete Sweeper).
type LogArchiveSyncer interface {
	// Sync performs one tick under the sweep's single-flight lock. The handler
	// checks InProgress first and answers 409 itself, so a concurrent-tick error
	// here is only logged.
	Sync(ctx context.Context, reconcile bool, actor string) error
	InProgress() bool
}

// New builds the Server.
func New(o Options) *Server {
	runCtx := o.Context
	if runCtx == nil {
		runCtx = context.Background()
	}
	s := &Server{
		cfg:                    o.Config,
		log:                    o.Logger,
		auth:                   o.Auth,
		db:                     o.DB,
		readyChecks:            o.ReadyChecks,
		webFS:                  o.WebFS,
		build:                  o.Build,
		runCtx:                 runCtx,
		shutdownWG:             o.ShutdownWG,
		scheduleReload:         o.ScheduleReload,
		scheduleForceReload:    o.ScheduleForceReload,
		scheduleTimezoneReload: o.ScheduleTimezoneReload,
		logDirChanged:          o.LogDirChanged,
		logArchiveSync:         o.LogArchiveSync,
		logArchiveChanged:      o.LogArchiveChanged,
	}
	// RB-7: point the role registry at THIS server's database. The registry is a
	// process-wide cache (requirePerm is mount-time middleware with no context and
	// no DB handle), so binding it here rather than only at boot matters for tests:
	// several servers are constructed per process against different temp databases,
	// and without this each would inherit whatever the previous test's roles were.
	// A failure keeps the compiled-in built-ins — see RefreshRoles.
	if s.db != nil {
		if err := auth.RefreshRoles(runCtx, s.db); err != nil && s.log != nil {
			s.log.Error("could not load roles; using the built-in permission matrix", "error", err)
		}
	}
	// Seed the cached effective app zone from the stored setting so schedule
	// projections agree with the cron engine from the first request (§4.4). A read
	// failure (or no DB in tests) leaves it to fall back to time.Local.
	if o.DB != nil {
		if gs, err := settings.GetGlobalSettings(runCtx, o.DB); err == nil {
			s.appLoc.Store(settings.ResolveEffectiveTimezone(gs))
		}
	}
	// Probe-only SSH service for "Test connection" (ssh-update.md TC.4). The
	// git-cache / log dirs are unused by probes, so they stay empty here; the
	// executor instance that needs them is built separately in mountRunners.
	if o.DB != nil {
		s.sshProbe = sshexec.New(o.DB, o.Config, o.Logger, "", "")
	}
	// Feed the active_runners gauge a DB-backed count (computed on each scrape).
	if o.DB != nil {
		db := o.DB
		metrics.SetRunnerCountFunc(func() float64 {
			var n float64
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// Count non-offline runners (online + draining) so a reaped runner
			// drops out of the gauge immediately (R3.3). A draining runner is
			// still alive and finishing work, so it counts as active.
			_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runners WHERE status != 'offline'`).Scan(&n)
			return n
		})
	}
	return s
}

// appLocation returns the cached effective application timezone — the zone the
// cron engine fires in — so next-run/upcoming projections agree with actual
// fires (§4.4). Never returns nil: falls back to time.Local when uncached.
func (s *Server) appLocation() *time.Location {
	if loc := s.appLoc.Load(); loc != nil {
		return loc
	}
	return time.Local
}

// logDirValue returns the effective run-log directory as the long-lived writers
// currently see it, falling back to a DB read before mountRunners has run (in
// practice, only in tests that build a Server without mounting).
func (s *Server) logDirValue(ctx context.Context) string {
	if p := s.logDir.Load(); p != nil {
		return *p
	}
	return settings.ResolveLogDir(ctx, s.db)
}

// applyLogDir re-points every run-log writer at dir (LU-5).
//
// Before this, the runner and SSH executor resolved the directory once at mount
// while writeSSHTestLog resolved it per request — so after a settings change the
// three disagreed until a restart, and ResolveLogDir's own doc comment conceded
// "path changes take effect on restart". They now share one in-process value.
//
// In-flight runs are unaffected: each keeps writing to the file handle it has
// already opened, and only the next run lands in the new directory. Existing
// logs are deliberately NOT moved — the read path resolves against the current
// directory, so a historical log stays readable only if the operator moves the
// tree themselves. That is the same trade the restart-based behaviour made.
func (s *Server) applyLogDir(ctx context.Context, dir string) {
	if dir == "" {
		return
	}
	s.logDir.Store(&dir)
	if s.runnerSvc != nil {
		s.runnerSvc.SetLogDir(dir)
	}
	if s.sshExec != nil {
		s.sshExec.SetLogDir(dir)
	}
	if s.logDirChanged != nil {
		s.logDirChanged(ctx, dir)
	}
}

// applyLogArchive rebuilds the archive Store from the stored log-storage
// settings (SL-1). A local backend clears it. A build failure keeps the previous
// store and logs — the settings save already probed the bucket, so a failure
// here is a decrypt or config error rather than a bad operator choice, and
// dropping the working client would make archived logs unreadable for no gain.
func (s *Server) applyLogArchive(ctx context.Context) {
	store, err := settings.BuildLogArchive(ctx, s.db, s.cfg, s.log)
	if err != nil {
		s.log.Error("log archive: could not build the S3 client from stored settings; keeping the previous one", "error", err)
		return
	}
	s.logArchive.Store(store)
	if s.logArchiveChanged != nil {
		s.logArchiveChanged()
	}
}

// LogArchive returns the current archive Store, or nil when the backend is
// local. Consumers must call this per use, never cache the result.
func (s *Server) LogArchive() *logarchive.Store { return s.logArchive.Load() }

// Handler returns the fully-wired http.Handler with global middleware applied.
func (s *Server) Handler() http.Handler {
	return s.withBaseMiddleware(s.buildMux())
}

// buildMux assembles the route table. Split from Handler so the spec
// conformance test can match routes against openapi.yaml directly.
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Health & readiness — unauthenticated, served at root, not under /api/v1 (T13).
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /version", s.version)

	// Runner-agent binary distribution (provisioning D1) — unauthenticated at
	// root by design (see agents.go): allowlisted filenames only, served from
	// CRONOMICON_AGENT_DIR, 404 with a build-it-yourself hint when not bundled.
	mux.HandleFunc("GET /agents/{filename}", s.handleAgentDownload)

	// One-click install script (provisioning plan 2 Phase 2) — unauthenticated
	// at root by design (see install.go): serves runner-install.sh with the
	// server URL + token baked in for a flagless `curl … | sudo bash`. A dumb
	// substitution endpoint; registration remains the sole enforcement point.
	mux.HandleFunc("GET /install/{token}", s.handleInstallScript)

	// Prometheus scrape target (C.2). Defaults preserve decision #9 —
	// unauthenticated at /metrics, reached on the internal network, NOT exposed
	// via the public proxy route. The DB-backed observability settings can
	// disable it or require a bearer token (checked per scrape, applies live);
	// a custom path is resolved once at startup.
	metricsPath := "/metrics"
	if s.db != nil {
		metricsPath = settings.ResolveMetricsPath(context.Background(), s.db)
	}
	mux.Handle("GET "+metricsPath, s.guardMetrics(metrics.Default().Handler()))

	// API v1 surface. Each feature slice owns one mount file (B3–B6); the router
	// just calls them. Slices meet on shared DB tables, not on each other's Go
	// packages, so they can be built independently and in parallel.
	s.mountAuth(mux)                 // B2: /me, /auth/*, /access/recent-logins
	s.mountGit(mux)                  // B3: /schedules/*, /git/*, /webhooks/gitlab, scopes-from-git
	s.mountRunners(mux)              // B4: /runners/*, /runs/{traceId}/log (ingest + stream)
	execEng := s.mountExecution(mux) // B5: /jobs/*, /workflows/*, /runs, /history, /activity, /change-log
	s.mountScripts(mux)              // B-Git: /scripts, /scripts/{name} (read-only catalog)
	s.mountPendingRuns(mux)          // AR: DELETE /pending-runs/{id} (cancel a deferred ad-hoc run)
	s.mountScheduleDefs(mux)         // A10a: /schedule-defs, /schedule-defs/{name} (first-class Schedules catalog)
	s.mountScheduleCompose(mux)      // schedule-builder.md: POST/PUT/DELETE /schedule-defs (in-app amadeus-source Schedule authoring; Compose RBAC)
	s.mountCalendars(mux)            // CAL-4: /calendars/* (working calendars — holiday skip / run-day sets bound to schedule entries)
	s.mountReactions(mux)            // RX-12: /reactions/* (a definition runs when another definition finishes)
	s.mountFileSightings(mux)        // FX-E1: GET /jobs/{jobId}/file-sightings (the arrival ledger's read surface)
	s.mountJobCompose(mux)           // A11: POST/PUT/DELETE /jobs (in-app amadeus-source Job composition; Compose RBAC)
	s.mountWorkflowCompose(mux)      // A11/Phase 4: POST/PUT/DELETE /workflows (in-app amadeus-source Workflow composition)
	s.mountSettings(mux)             // B6: /env-vars, /secrets, /scopes, /alerts, /ssh-hosts, /settings, /audit/export
	s.mountBindings(mux)             // vault-integration.md P1.1: /{job,script}-reference-bindings/*, /script-reference-scan (explicit reference bindings)
	s.mountAccess(mux)               // LB3: /roles, /ad-group-mappings/*, /scope-restrictions
	s.mountAccessGrants(mux)         // RB-18: /access-grants (INERT — nothing authorizes on it yet)
	s.mountAgencies(mux)             // agency-support.md M1: /agencies/*, /scopes/{id}/agency (network-isolation zones)
	// ET-A/B: /service-accounts (machine principals) + /trigger/* (the only
	// token-authenticated, session-less route family). Shares mountExecution's
	// engine so a workflow triggered by a token is cancellable like any other.
	s.mountServiceAccounts(mux, execEng)
	// RH: /definitions/*/revisions + /recycle-bin (in-app definition history
	// and undelete; Git-source definitions get both from Git).
	s.mountRevisions(mux)
	// SL-E: /analytics/runs (success rate, duration percentiles, failure clustering).
	s.mountAnalytics(mux)

	// Frontend static assets, served by the same binary (T2/T3). SPA fallback
	// to index.html keeps client-side routing working.
	if s.webFS != nil {
		mux.Handle("/", s.spaHandler())
	}

	return mux
}

// guardMetrics applies the live observability gate to the scrape endpoint:
// 404 when disabled, 401 unless the bearer token matches when authType=bearer.
// With no DB (tests) or default settings it passes through untouched (#9).
func (s *Server) guardMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.db == nil || s.cfg == nil {
			next.ServeHTTP(w, r)
			return
		}
		g := settings.GetMetricsGuard(r.Context(), s.db, s.cfg)
		if !g.Enabled {
			http.NotFound(w, r)
			return
		}
		if g.AuthType == "bearer" {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if g.BearerToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(g.BearerToken)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withBaseMiddleware applies cross-cutting concerns: request logging, a panic
// recovery boundary, and — critically — trusted-proxy header stripping (A.3),
// which runs before any handler so spoofed Remote-* identity headers from an
// untrusted peer can never be honored. Auth/CSRF are applied per-route, not
// globally, because health and runner endpoints have different auth than
// operator ones.
func (s *Server) withBaseMiddleware(next http.Handler) http.Handler {
	// limitBody is innermost (closest to the mux) so the body cap is in place
	// before any handler reads r.Body (PP-M1).
	h := s.limitBody(next)
	if s.auth != nil {
		h = s.auth.StripUntrustedHeaders(h)
	}
	// metricsMiddleware wraps the mux so r.Pattern (the matched route) is
	// populated when it records; requestLogger/recoverer sit outside it.
	return s.recoverer(s.requestLogger(s.metricsMiddleware(h)))
}
