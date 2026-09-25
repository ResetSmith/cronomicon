// Package runner implements the B4 runner subsystem: registration, long-poll
// work dispatch, log streaming (T6), redaction (T7/S7), drain (A6.4), and
// single-use registration-token mint/list/revoke (§6.6, Phase 7).
//
// The DB is the bus: this package reads the runs table (owned by
// B5) but only transitions rows it has claimed (queued→running, running→terminal).
// It never creates queued runs — that is B5's job.
package runner

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

const (
	// DefaultLogDir is the fallback per-run log directory, used until the
	// settings-resolved path is pushed in via SetLogDir (and by tests).
	DefaultLogDir = "/var/lib/amadeus/logs"

	// pollTimeout is the server-side long-poll timeout (A6.2).
	pollTimeout = 30 * time.Second

	// pollInterval is how often the long-poll loop re-checks for work.
	pollInterval = 500 * time.Millisecond

	// drainDefaultMinutes is the default drain timeout (A6.4).
	drainDefaultMinutes = 60

	// tokenPrefix is the prefix for shared registration tokens (A6.1).
	tokenPrefix = "crn_reg_" //nolint:gosec // G101: a token-format prefix, not a credential

	// runnerTokenPrefix is the prefix for per-runner API keys.
	runnerTokenPrefix = "crn_run_"

	// tokenBytes is the number of random bytes in a generated token.
	tokenBytes = 32

	// driftOpCooldown bounds how often the Phase-5 drift detector may deliver
	// a re-register op to one runner — a flapping agent (one that keeps
	// polling with a mismatched digest after redeclaring) must not
	// re-register-loop. Generous vs the ~60s poll cadence: several polls'
	// worth of convergence time before a repeat is allowed.
	driftOpCooldown = 5 * time.Minute
)

// Service holds dependencies for the runner subsystem handlers.
type Service struct {
	db       *sql.DB
	cfg      *config.Config
	log      *slog.Logger
	resolver *runref.Resolver // dispatch-time reference injection for the manifest (P1.4)
	// logDir is the per-run log directory. Held atomically rather than as a
	// plain field because the operator can re-point it while runs are in flight
	// (LU-5) — every read is on a live request path (logPath/ensureLogDir), so a
	// plain string here would be a data race against the settings handler.
	logDir     atomic.Pointer[string]
	notifier   notify.Notifier // optional (C.1); nil ⇒ no dispatch
	shutdownWG *sync.WaitGroup // optional (PP-L15); tracks the reaper goroutine for graceful drain

	// authSvc records authorization denials to the auth audit trail (LU-9). This
	// package owns exactly one operator-facing authz decision — the scope gate on
	// HandleGetLog — and it must land in the same table as internal/api's, or the
	// one place an out-of-scope actor can read another team's run OUTPUT would be
	// the one denial an auditor never sees. Optional: the reaper instance in main
	// serves no HTTP, so nil ⇒ the guard still denies, it just does not audit.
	authSvc *auth.Service
	// logArchive returns the S3 archive store, nil while the backend is local
	// (SL-3). Called per read, never cached — the api rebuilds the store after a
	// settings save. Wired by mountRunners from Server.LogArchive.
	logArchive func() *logarchive.Store

	// driftLastOp is the Phase-5 flap guard: per-runner timestamp of the last
	// re-register op delivered (drift-detected or manual Resync). In-memory by
	// design — best-effort rate limiting; a server restart clearing it is fine.
	driftMu     sync.Mutex
	driftLastOp map[string]time.Time
}

// New creates a runner.Service from the shared server dependencies.
func New(db *sql.DB, cfg *config.Config, log *slog.Logger) *Service {
	// Vault-wire the resolver's Service so vault-source secrets and SSH credentials
	// (P2.4) resolve in the manifest through the same configured client the API
	// server uses; unconfigured ⇒ the stub, unchanged behavior.
	sec := settings.WireVaultClient(context.Background(), db, cfg, secrets.New(db, cfg, log), log)
	s := &Service{
		db:          db,
		cfg:         cfg,
		log:         log,
		resolver:    runref.NewResolver(db, cfg, sec, log),
		driftLastOp: map[string]time.Time{},
	}
	s.SetLogDir(DefaultLogDir)
	return s
}

// SetLogDir points the service at a run-log directory. Safe to call while
// requests are in flight: in-flight writes finish against the handle they
// already opened, and the next run lands in the new directory (LU-5).
func (s *Service) SetLogDir(dir string) {
	if dir == "" {
		dir = DefaultLogDir
	}
	s.logDir.Store(&dir)
}

// LogDir returns the current run-log directory.
func (s *Service) LogDir() string {
	if p := s.logDir.Load(); p != nil {
		return *p
	}
	return DefaultLogDir
}

// WithAuthAudit wires the auth service used to record authorization denials
// (LU-9). Chainable and optional, mirroring WithNotifier: only the instance that
// actually serves HTTP needs it.
// WithLogArchive wires the archive-store getter HandleGetLog falls back to when
// a run's local log is gone but the run row says it was archived (SL-3).
func (s *Service) WithLogArchive(fn func() *logarchive.Store) *Service {
	s.logArchive = fn
	return s
}

func (s *Service) WithAuthAudit(a *auth.Service) *Service {
	s.authSvc = a
	return s
}

// WithNotifier wires a notification dispatcher invoked on terminal runs (C.1).
func (s *Service) WithNotifier(n notify.Notifier) *Service {
	s.notifier = n
	return s
}

// WithShutdownWG registers a WaitGroup the reaper goroutine joins so graceful
// shutdown can drain it before the DB pool closes (PP-L15). Chainable.
func (s *Service) WithShutdownWG(wg *sync.WaitGroup) *Service {
	s.shutdownWG = wg
	return s
}

// generateToken mints a random token with the given prefix.
// The hex tail is 64 chars (32 bytes = 256 bits).
func generateToken(prefix string) (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// now returns the current time formatted as RFC3339 (ISO-8601, S12).
func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ErrBadTraceID rejects a trace ID that cannot safely become a filename.
var ErrBadTraceID = errors.New("invalid trace id")

// maxTraceIDLen bounds the filename component. A UUIDv7 is 36 chars; the
// headroom is for the `<code>-<uuidv7>` form LU-Q9(c) leaves open.
const maxTraceIDLen = 64

// ValidTraceID reports whether id is safe to use as a log-file name component
// (LU-12). Trace IDs arrive as unvalidated path parameters and are concatenated
// straight into a filesystem path; nothing else in the codebase checks their
// shape, and openapi's `format: uuid` is documentation, not enforcement.
//
// Go's ServeMux `{traceId}` wildcard is single-segment, so a "/" cannot reach
// here through the router — but that is a property of the current routing, not
// of this function's callers, and a literal ".." is not foreclosed by it at all.
//
// The check is deliberately shape-based (no separators, no dot-dot, bounded
// length, a conservative character class) rather than a UUID match. A UUID
// regex would be tighter but would reject the short literal IDs used throughout
// the existing tests ("run-1", "r1", "run-bash") and would have to be relaxed
// again the moment the trace-ID format changes — while adding nothing to the
// traversal defence, which is what this is for.
func ValidTraceID(id string) bool {
	if id == "" || len(id) > maxTraceIDLen {
		return false
	}
	if strings.Contains(id, "..") || id[0] == '.' {
		return false
	}
	for i := 0; i < len(id); i++ {
		ch := id[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == '.':
		default:
			return false
		}
	}
	return true
}

// logPath returns the path to the per-run log file for a run whose stamped
// entity code is entityCode (empty ⇒ the pre-710 flat layout). See LogPath.
func (s *Service) logPath(entityCode, traceID string) (string, error) {
	return LogPath(s.LogDir(), entityCode, traceID)
}

// ensureLogDir creates the directory a run's log will be written into.
func (s *Service) ensureLogDir(entityCode string) error {
	return EnsureLogDir(s.LogDir(), entityCode)
}

// sessionActor names the human behind an operator-initiated runner action, for
// the `actor` of the activity row it writes (AA-4).
//
// The three runner-lifecycle handlers — drain, host-key scan request, resync
// request — wrote the LITERAL string "operator" while holding a session, so
// "who drained runner X?" was unanswerable from the feed: the identity was in
// scope and thrown away. `operator` was only ever meant as the no-session
// fallback, which is how the host-key APPROVAL handler in keyscan.go had it
// right all along; this is that code, lifted so there is one copy.
//
// The fallback is kept rather than 401ing: these routes are session-gated at
// the mux, so it cannot fire in real traffic, and a writer that cannot know
// the actor should say "operator" rather than assert a name it guessed.
func sessionActor(r *http.Request) string {
	if idn, ok := auth.IdentityFrom(r.Context()); ok && idn.Email != "" {
		return idn.Email
	}
	return "operator"
}

// runnerNameOf resolves a runner's display name for the `runner_name` snapshot
// on an activity row (AA-1). Best effort by design: a runner deregistered
// between the event and this lookup yields "", the row carries NULL, and the
// AA-2 filter simply will not match it — which is the honest answer, and the
// same one migration 1100's backfill gives for a row it cannot resolve.
//
// Callers that ALREADY hold the name (the poll re-admit notice, resync's
// declared name, the drain/keyscan handlers' runner row) must pass it directly
// rather than call this — a second query for a value in scope is waste.
func (s *Service) runnerNameOf(ctx context.Context, runnerID string) string {
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name)
	return name
}
