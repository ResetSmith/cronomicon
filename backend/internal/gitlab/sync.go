package gitlab

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/inventory"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/watchspec"
	"github.com/ResetSmith/cronomicon/internal/workflow"
	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	plumbing "github.com/go-git/go-git/v5/plumbing"
	filemode "github.com/go-git/go-git/v5/plumbing/filemode"
	object "github.com/go-git/go-git/v5/plumbing/object"
	githttpclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"gopkg.in/yaml.v3"
)

// guardedGitTransportOnce ensures the go-git HTTP transport is installed once —
// InstallProtocol is a process-global registry mutation.
var guardedGitTransportOnce sync.Once

// installGuardedGitTransport gives go-git's clone/fetch/push an HTTP client with
// the SU-7 SSRF egress guard, a response-header timeout, and redirect refusal
// (SU-8) — go-git otherwise uses an unbounded, unguarded default client, the one
// outbound path with none of the three. Local (file://) remotes used by tests use
// a different transport and are unaffected.
func (s *Service) installGuardedGitTransport() {
	guardedGitTransportOnce.Do(func() {
		pol := httpx.EgressPolicy{AllowPrivate: true} // safe default when Cfg is absent
		if s.Cfg != nil {
			pol = httpx.EgressPolicy{AllowPrivate: s.Cfg.OutboundAllowPrivate, AllowLoopback: s.Cfg.OutboundAllowLoopback}
		}
		tr := httpx.SafeTransport(nil, pol)
		tr.ResponseHeaderTimeout = 30 * time.Second // bound a hung server, without capping a large transfer
		guarded := &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		gt := gogithttp.NewClient(guarded)
		githttpclient.InstallProtocol("https", gt)
		githttpclient.InstallProtocol("http", gt)
	})
}

// Service manages the Git clone and drives sync operations.
type Service struct {
	db       *sql.DB
	log      *slog.Logger
	cloneDir string // path to the local git clone
	repoURL  string // full GitLab HTTPS URL
	// repoID is the repository this Service syncs (GR-3). Empty means Global's,
	// which is the only repository an installation has until the repositories
	// table arrives (2.4.0, Phase R2); read it through repo(). It is stamped on
	// every script this Service writes and bounds the scripts it prunes.
	repoID string
	// agencyID is the agency that repository belongs to (GR-1): every row it
	// supplies is that agency's. Empty means Global, for the same reason; read
	// it through agency(). It is stamped on the schedules this Service writes
	// (schedules.owner_agency, migration 1300).
	agencyID      string
	token         string // CRONOMICON_GITLAB_TOKEN (may be empty — unauthenticated)
	webhookSecret string
	Cfg           *config.Config // added for KEK-based webhook secret decryption

	// gate is the one queue a sync, a blocking sync and a publish wait in: the
	// Registry's, shared by the Services of every repository (GR-11). nil for a
	// Service built on its own (tests), which gets one of its own from queue().
	gate     chan struct{}
	gateOnce sync.Once
	// ctx is the Service's lifetime: cancelled by Stop, and by the end of the
	// process. Everything the Service does on its own account (the syncs
	// TriggerSync starts) runs on it, and everything it does for a caller is
	// cancelled with it. nil for a Service built on its own; read it through
	// lifetime().
	ctx    context.Context
	cancel context.CancelFunc
	// wg counts the operations in flight, so that Stop can wait for them.
	wg sync.WaitGroup

	mu      sync.Mutex // guards syncing, pending, pendingTrigger and stopped
	syncing bool       // a sync started by TriggerSync is queued or running
	// pending: a trigger arrived while that sync was queued or running. One more
	// sync follows it, with pendingTrigger as its label.
	pending        bool
	pendingTrigger string
	stopped        bool

	// afterFetch, when set, is called once the clone is at the remote's commit
	// and before anything is parsed. Tests use it to act "during a sync".
	afterFetch func()

	// problems collects what the sync that is under way has to say about the
	// repository's files (GR-30, problems.go). nil between syncs. One sync of a
	// Service runs at a time (the queue), so there is one collector at a time;
	// but a warning can be logged by a goroutine that is not the one syncing
	// (the line that follows a sync started by a trigger), so the pointer is
	// read and set under problemsMu.
	problemsMu sync.Mutex
	problems   *problemSet

	// onSyncComplete, when set, is invoked at the end of a sync that committed,
	// with the definitions generation that sync advanced to, so the scheduler can
	// reload newly-synced schedules immediately (it compares the generation with
	// the one it loaded at). Optional — nil is a no-op.
	onSyncComplete func(ctx context.Context, generation string)
}

// SetOnSyncComplete installs the post-sync hook (wired in main.go to the
// scheduler's generation-gated reload). Passing a func value avoids an
// api→scheduler import edge.
func (s *Service) SetOnSyncComplete(f func(ctx context.Context, generation string)) {
	s.onSyncComplete = f
}

// NewService builds the GitLab Service.
//
// cloneDir should be a persistent path (e.g. /var/lib/cronomicon/git-cache/job-definitions).
// repoURL/token are resolved by the caller (settings.ResolveGitlabRuntime:
// env first, DB-backed config second). If token is empty, clones are attempted
// unauthenticated — this works for public repos; private repos will fail (the
// error surfaces in the sync result).
func NewService(db *sql.DB, log *slog.Logger, repoURL, token, cloneDir, webhookSecret string) *Service {
	return &Service{
		db:            db,
		log:           log,
		cloneDir:      cloneDir,
		repoURL:       repoURL,
		token:         token,
		webhookSecret: webhookSecret,
	}
}

// ValidateWebhookToken validates a webhook token against THIS repository's
// secret set (its row of git_repos).
// If CRONOMICON_GITLAB_WEBHOOK_SECRET is set, only that secret is accepted.
// Otherwise, it checks the active secret and, if the overlap window has not expired,
// the previous secret stored in the database (encrypted using KEK).
//
// The environment's secret and the secret read at start-up are Global's
// repository's alone (GR-21): they are scalar, and a second repository that
// honoured them would open its webhook to whoever holds Global's secret.
func (s *Service) ValidateWebhookToken(ctx context.Context, token string) bool {
	// ctEq is a local alias for constant-time string comparison to prevent
	// timing-based webhook secret enumeration (PP-L10).
	ctEq := func(a, b string) bool {
		return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
	}

	// 1. Env var override check
	if envSecret := settings.EnvWebhookSecret(s.repo()); envSecret != "" {
		return ctEq(token, envSecret)
	}
	bootSecret := ""
	if s.repo() == repoid.Global {
		bootSecret = s.webhookSecret
	}

	// 2. Read secrets from database
	row := s.db.QueryRowContext(ctx, `
		SELECT webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until
		FROM git_repos WHERE id=?`, s.repo())
	var secretEnc, prevEnc, overlapUntil sql.NullString
	if err := row.Scan(&secretEnc, &prevEnc, &overlapUntil); err != nil {
		// Fallback to static webhookSecret loaded at boot if DB query fails or has no rows
		return bootSecret != "" && ctEq(token, bootSecret)
	}

	// 3. Decrypt and check active secret
	if secretEnc.Valid && secretEnc.String != "" && s.Cfg != nil {
		decrypted, err := secrets.DecryptString(s.Cfg, secretEnc.String)
		if err == nil && ctEq(token, decrypted) {
			return true
		}
	} else if bootSecret != "" && ctEq(token, bootSecret) {
		// If DB has no active secret but boot secret matches, accept it
		return true
	}

	// 4. Decrypt and check previous secret if within overlap window
	if prevEnc.Valid && prevEnc.String != "" && overlapUntil.Valid && overlapUntil.String != "" && s.Cfg != nil {
		expiry, err := time.Parse(time.RFC3339, overlapUntil.String)
		if err == nil && time.Now().UTC().Before(expiry) {
			decryptedPrev, err := secrets.DecryptString(s.Cfg, prevEnc.String)
			if err == nil && ctEq(token, decryptedPrev) {
				return true
			}
		}
	}

	return false
}

// repo is the id of the repository this Service syncs: Global's, unless the
// Service was built for another.
func (s *Service) repo() string {
	if s.repoID == "" {
		return repoid.Global
	}
	return s.repoID
}

// agency is the id of the agency this Service's repository belongs to:
// Global, unless the Service was built for another's.
func (s *Service) agency() string {
	if s.agencyID == "" {
		return agencyid.Global
	}
	return s.agencyID
}

// SyncResult carries the outcome of a single sync attempt.
type SyncResult struct {
	SHA             string
	Status          string // "success" | "failed" | "partial"
	JobsSynced      int
	ScriptsSynced   int
	SchedulesSynced int
	WfsSynced       int
	ScopesSynced    int
	ErrorMessage    string
	StartedAt       time.Time
	FinishedAt      time.Time
	Errors          []ValidationError
	// notRun: the sync was never attempted, because the Service was stopped or
	// the caller went away while it waited its turn. Status is "failed", and no
	// history row was written: nothing happened to record.
	notRun bool
}

// TriggerSync starts an async sync and returns immediately. At most one sync
// of this repository started this way is queued or running at a time. A
// trigger that arrives meanwhile is not dropped: it is remembered, and ONE
// more sync runs after the current one, however many arrived.
//
// Until 2.4.0 such a trigger was dropped ("coalesced"). The sync it was
// coalesced into may already have fetched, so a push that arrived during a
// sync was picked up by nothing until the next delivery or a manual sync: no
// timer polls the repository.
func (s *Service) TriggerSync(_ context.Context, triggeredBy string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.syncing {
		s.pending, s.pendingTrigger = true, triggeredBy
		s.mu.Unlock()
		s.logInfo("sync already queued or running — one more will follow it", "trigger", triggeredBy)
		return
	}
	s.syncing = true
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		for trig := triggeredBy; ; {
			s.syncLogged(trig)
			s.mu.Lock()
			again := s.pending && !s.stopped
			trig = s.pendingTrigger
			s.pending, s.pendingTrigger = false, ""
			if !again {
				s.syncing = false
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
		}
	}()
}

// syncLogged runs one sync on the Service's own account and logs how it went.
func (s *Service) syncLogged(triggeredBy string) {
	defer func() {
		if r := recover(); r != nil {
			s.logError("git sync panicked", "panic", r)
		}
	}()
	res := s.sync(s.lifetime(), triggeredBy)
	switch {
	case res.Status == "success":
		s.logInfo("git sync complete",
			"repo_id", s.repo(),
			"sha", res.SHA,
			"jobs", res.JobsSynced,
			"scripts", res.ScriptsSynced,
			"schedules", res.SchedulesSynced,
			"workflows", res.WfsSynced,
			"scopes", res.ScopesSynced)
	case res.notRun, s.lifetime().Err() != nil:
		// The Service was stopped while the sync waited its turn, or part of the
		// way through it: its repository was disconnected, its connection
		// rewritten, or the server is shutting down. Nothing is wrong.
	case s.repoURL == "":
		// GitLab is an optional dependency that degrades gracefully (T12):
		// an unconfigured/unreachable remote is a warning, not an error.
		s.logWarn("git sync skipped — GitLab not configured", "repo_id", s.repo(), "status", res.Status)
	default:
		s.logError("git sync failed", "repo_id", s.repo(), "status", res.Status, "error", res.ErrorMessage)
	}
}

// lifetime is the context the Service lives on: its own, or the background
// context for a Service built on its own.
func (s *Service) lifetime() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// queue is the gate this Service waits in: the Registry's, or one of its own.
func (s *Service) queue() chan struct{} {
	s.gateOnce.Do(func() {
		if s.gate == nil {
			s.gate = make(chan struct{}, 1)
		}
	})
	return s.gate
}

// errServiceStopped is what an operation asked of a stopped Service gets.
var errServiceStopped = errors.New("the repository's sync service has been stopped")

// enter queues one operation on the clone (a sync, a publish) and returns when
// it is this operation's turn: no other operation of ANY repository's Service
// is then running (GR-11). The context it returns ends when the caller's does
// or when the Service is stopped, whichever is first. release must be called
// when the operation is done.
//
// It fails, having started nothing, when the Service is stopped or either
// context ends before the turn comes.
func (s *Service) enter(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, nil, errServiceStopped
	}
	s.wg.Add(1)
	s.mu.Unlock()

	opCtx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(s.lifetime(), cancel)
	gate := s.queue()
	leave := func() error {
		unhook()
		cancel()
		s.wg.Done()
		if s.lifetime().Err() != nil {
			return errServiceStopped
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return errServiceStopped
	}
	select {
	case gate <- struct{}{}:
	case <-opCtx.Done():
		return nil, nil, leave()
	}
	if opCtx.Err() != nil {
		// The turn came at the moment the Service was stopped or the caller
		// went away: give it up rather than start work that is already cancelled.
		<-gate
		return nil, nil, leave()
	}
	return opCtx, func() {
		<-gate
		unhook()
		cancel()
		s.wg.Done()
	}, nil
}

// Stop ends the Service: it starts nothing more, whatever it has in flight is
// cancelled, and Stop returns when that has unwound. A cancelled sync leaves
// the database as it was (its transaction rolls back) and the clone as far as
// the fetch got, which the next Service of the repository repairs by fetching
// and resetting, as every sync does.
func (s *Service) Stop() {
	s.halt()
	s.wg.Wait()
}

// halt is Stop without the wait: the Service starts nothing more and whatever
// it has in flight is cancelled, and halt returns at once. Parts of a sync
// cannot be interrupted (parsing, the reset of the working tree), so "at once"
// and "unwound" are different moments. The registry replaces a Service with
// halt: the successor cannot touch the clone before the predecessor has left
// it, because both wait in the one queue.
func (s *Service) halt() {
	s.mu.Lock()
	s.stopped = true
	s.pending, s.pendingTrigger = false, ""
	s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

// logInfo logs at Info level, safely handling a nil logger.
func (s *Service) logInfo(msg string, args ...any) {
	if s.log != nil {
		s.log.Info(msg, args...)
	}
}

// logError logs at Error level, safely handling a nil logger.
func (s *Service) logError(msg string, args ...any) {
	if s.log != nil {
		s.log.Error(msg, args...)
	}
}

// heldBoundScopes names the git scopes this sync would prune but must keep for
// now: gone from Git (synced_at older than this pass), bound to runners, and
// with runs still waiting under their name (settings.BoundScopeBusySQL). Purely
// for the warning — the prune statements apply the same predicate themselves.
// A read error yields nil: the predicate in the DELETE is what protects the
// rows, and a missing log line must not fail a sync.
func heldBoundScopes(ctx context.Context, tx *sql.Tx, nowStr, repoID string) []string {
	rows, err := tx.QueryContext(ctx, `
		SELECT name FROM scopes
		 WHERE synced_at < ? AND source = 'git' AND repo_id = ? AND `+settings.BoundScopeBusySQL+`
		 ORDER BY name`, nowStr, repoID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// logWarn logs at Warn level, safely handling a nil logger.
//
// During a sync it is also a row of the repository's problems (noteWarning):
// whatever a sync warns of is told to the repository's agency, not only to
// whoever reads the server's log.
func (s *Service) logWarn(msg string, args ...any) {
	s.noteWarning(msg, args)
	if s.log != nil {
		s.log.Warn(msg, args...)
	}
}

// SyncBlocking runs a sync synchronously and returns the result. Used by the
// operator-triggered POST /api/v1/git/sync.
func (s *Service) SyncBlocking(ctx context.Context, triggeredBy string) SyncResult {
	return s.sync(ctx, triggeredBy)
}

// sync waits its turn in the queue and runs one sync.
func (s *Service) sync(ctx context.Context, triggeredBy string) SyncResult {
	opCtx, release, err := s.enter(ctx)
	if err != nil {
		now := time.Now().UTC()
		return SyncResult{Status: "failed", ErrorMessage: "sync not run: " + err.Error(), StartedAt: now, FinishedAt: now, notRun: true}
	}
	defer release()
	return s.syncNow(opCtx, triggeredBy)
}

// syncNow is the sync itself. The caller holds the queue.
func (s *Service) syncNow(ctx context.Context, triggeredBy string) SyncResult {
	// LR-78: a sync may add, rename away or prune a scope, and a scope's existence
	// decides what an agency grant reaches. The grant snapshot is told on the way
	// out, after the transaction has committed or rolled back. (gitlab cannot be
	// handed the auth Service; the counter is the hook.)
	defer auth.GrantsChanged()
	start := time.Now().UTC()
	res := SyncResult{Status: "failed", StartedAt: start}
	if s.db == nil {
		res.ErrorMessage = "db not configured"
		res.FinishedAt = time.Now().UTC()
		return res
	}

	branch := s.writeBranch(ctx)
	// From here to the end of the sync, whatever it warns of or reports is
	// collected for the repository's problem rows (GR-30).
	s.setProblems(&problemSet{})
	defer s.setProblems(nil)
	repo, err := s.cloneOrFetch(ctx, branch)
	if err != nil {
		res.ErrorMessage = err.Error()
		res.FinishedAt = time.Now().UTC()
		_ = s.recordSyncEvent(ctx, triggeredBy, res)
		// Not for a sync cut short because its Service was stopped. A repository
		// with no URL has nothing to fetch (the history says "not configured"),
		// and nothing to say about files nobody reads any more.
		switch {
		case s.repoURL == "":
			s.clearProblems(context.WithoutCancel(ctx))
		case s.lifetime().Err() == nil && ctx.Err() == nil:
			s.noteFetchFailure(ctx, err)
		}
		return res
	}
	if s.afterFetch != nil {
		s.afterFetch()
	}

	sha, err := s.headSHA(repo)
	if err != nil {
		res.ErrorMessage = "failed to read HEAD SHA: " + err.Error()
		res.FinishedAt = time.Now().UTC()
		_ = s.recordSyncEvent(ctx, triggeredBy, res)
		return res
	}
	res.SHA = sha

	// Parse scripts and schedules FIRST so job/workflow script_ref + scheduleRefs
	// can be resolved against them (B-Git / A10a). Order matters; the rest are
	// independent.
	scripts, scriptErrs := s.parseScripts()
	scheds, schedErrs := s.parseSchedules()
	jobs, jobErrs := s.parseJobs()
	wfs, wfErrs := s.parseWorkflows()
	scopes, scopeErrs := s.parseInventories()

	var allErrs []string
	collect := func(es []error) {
		for _, e := range es {
			allErrs = append(allErrs, e.Error())
			if ve, ok := e.(ValidationError); ok {
				res.Errors = append(res.Errors, ve)
			} else if ve, ok := e.(*ValidationError); ok && ve != nil {
				res.Errors = append(res.Errors, *ve)
			} else {
				res.Errors = append(res.Errors, ValidationError{Message: e.Error()})
			}
		}
	}
	collect(scriptErrs)
	collect(schedErrs)
	collect(jobErrs)
	collect(wfErrs)
	collect(scopeErrs)

	// A name that two files of this repository both define (present defect 19).
	// One of the two is used, the one read last, and until 2.4.0 the sync said
	// nothing: a clean success, and which file's definition ran was the order the
	// directory happened to be walked in. It is REPORTED now, per file, and the
	// sync is partial. It is not refused, and it switches no prune off: both
	// would change what a repository that has such a pair runs today.
	var dupErrs []ValidationError
	dupErrs = append(dupErrs, duplicateNames("script", scripts,
		func(x ScriptYAML) string { return x.Metadata.Name }, func(x ScriptYAML) string { return x.SourcePath })...)
	dupErrs = append(dupErrs, duplicateNames("schedule", scheds,
		func(x ScheduleYAML) string { return x.Metadata.Name }, func(x ScheduleYAML) string { return x.SourcePath })...)
	dupErrs = append(dupErrs, duplicateNames("job", jobs,
		func(x JobYAML) string { return x.Metadata.Name }, func(x JobYAML) string { return x.SourcePath })...)
	dupErrs = append(dupErrs, duplicateNames("workflow", wfs,
		func(x WorkflowYAML) string { return x.Metadata.Name }, func(x WorkflowYAML) string { return x.SourcePath })...)
	for _, ve := range dupErrs {
		allErrs = append(allErrs, ve.Error())
		res.Errors = append(res.Errors, ve)
	}

	// PP-B2: per-subsystem parse health. A subsystem's GitOps prune runs ONLY
	// when that subsystem parsed cleanly — directory readable AND zero parse /
	// resolve / cross-ref errors — so a transient read error, one corrupt file,
	// or a mid-rewrite tree can't delete the whole catalog of that kind. A
	// legitimately empty/absent directory parses clean (parse* returns nil,nil)
	// and is still pruned, which is a real "operator removed all defs" delete.
	scriptsOK := len(scriptErrs) == 0
	schedsOK := len(schedErrs) == 0
	jobsOK := len(jobErrs) == 0
	wfsOK := len(wfErrs) == 0
	scopesOK := len(scopeErrs) == 0

	// Resolve scripts (compute content hashes, reading any scriptPath files from
	// the clone) so jobs can denormalize the referenced script's run_type/body/
	// executor onto their cache row at sync time (Decision 7).
	resolved, resolveErrs := s.resolveScripts(scripts)
	collect(resolveErrs)
	scriptsOK = scriptsOK && len(resolveErrs) == 0

	// Phase 3 (RX.5/§6.2, P2): pinning-lint + tree secret-scan each checkout
	// project. At sync these are WARNINGS — never blocking (the same
	// never-blocking posture as the advisory columns). `cronomicon validate` / CI
	// turns the identical findings into hard errors.
	for _, sc := range scripts {
		if strings.TrimSpace(sc.Spec.ProjectRoot) == "" {
			continue
		}
		for _, f := range lintProjectWith(s.cloneDir, sc.Spec.ProjectRoot, s.readCloned) {
			s.logWarn("git sync: checkout project lint (advisory)",
				"project", sc.Spec.ProjectRoot, "file", f.File, "detail", f.Message)
		}
	}

	// Resolve first-class schedules (compute content hashes) so jobs/workflows can
	// expand their scheduleRefs into the runtime definition_schedules cache (A10a).
	resolvedScheds, schedResolveErrs := s.resolveSchedules(scheds)
	collect(schedResolveErrs)
	schedsOK = schedsOK && len(schedResolveErrs) == 0

	// What this repository's definitions take from Global's (GR-16, borrow.go):
	// a script, a schedule or a definition to react to that this repository has
	// no file for and the installation's own repository supplies.
	borrow := s.newBorrowing(ctx, scripts, scheds)
	// elsewhere is the end of a "does not resolve" message: where was looked.
	elsewhere := ""
	if s.repo() != repoid.Global {
		elsewhere = " of this repository, nor to one of the installation's own"
	}

	// dangleScheduleRefs reports any scheduleRef that resolves to no schedule, as a
	// hard sync error (caught at sync/MR time, not run time — same as script_ref).
	dangleScheduleRefs := func(file string, refs []string) bool {
		bad := false
		for _, ref := range refs {
			if _, ok := resolvedScheds[ref]; ok {
				continue
			}
			if _, ok := borrow.schedule(ref); !ok {
				ve := ValidationError{File: file, Field: "spec.scheduleRefs",
					Message: fmt.Sprintf("scheduleRef %q does not resolve to any schedules/*.yaml%s", ref, elsewhere)}
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
				bad = true
			}
		}
		return bad
	}

	// CAL-9 — validate every inline entry's calendar binding against the operator
	// authored catalogue, with the SAME refusals the in-app compose path applies
	// (calendar.ValidateBinding). Calendars are never Git-authored (CAL-Q2), so a
	// binding always names a row that must already exist here; an unknown name is
	// a hard sync error caught at MR time rather than a suppression that silently
	// never happens (skip) or an entry that silently never fires (only).
	//
	// The flag load is best-effort: if the calendars table cannot be read, binding
	// validation is skipped rather than failing the whole repo's sync. That
	// matches the window/mode precedent above — a bad spec must not wedge a sync —
	// and the fire-time gate is fail-open for the same reason.
	knownCals, globalCals, calErr := calendar.LoadFlags(ctx, s.db)
	if calErr != nil {
		s.logError("sync: load calendars for binding validation — bindings not validated", "err", calErr)
	}
	dangleCalendars := func(file string, entries []ScheduleEntry) bool {
		if calErr != nil {
			return false
		}
		bad := false
		for _, e := range entries {
			if _, _, verr := calendar.ValidateBinding(e.SkipCalendars, e.OnlyCalendars, knownCals, globalCals); verr != "" {
				ve := ValidationError{File: file, Field: "spec.schedules[" + e.Name + "].skipCalendars",
					Message: verr}
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
				bad = true
			}
		}
		return bad
	}

	// The SAME check for FIRST-CLASS schedules (schedules/*.yaml). Easy to miss,
	// and missing it defeats the point: a first-class schedule's binding is copied
	// down onto every entry that references it, so one unvalidated
	// `onlyCalendars: [typo]` silently stops N jobs from ever firing again. A
	// schedule that fails is dropped from the resolved set, which makes every
	// scheduleRef naming it dangle — the existing hard error for that case.
	if calErr == nil {
		for name, rs := range resolvedScheds {
			if _, _, verr := calendar.ValidateBinding(rs.skipCals, rs.onlyCals, knownCals, globalCals); verr != "" {
				ve := ValidationError{File: rs.sourcePath, Field: "spec.skipCalendars",
					Message: fmt.Sprintf("schedule %q: %s", name, verr)}
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
				delete(resolvedScheds, name)
				schedsOK = false
			}
		}
	}

	// RX-13 — reaction shape + cross-reference validation, mirroring the calendar
	// binding guard above. Two layers, for the same reason CAL split them:
	// NormalizeReactions catches everything checkable offline (name slug,
	// onOutcome enum, non-negative delays) and is ALSO run by `cronomicon validate`
	// at MR time; the upstream-exists check needs the DB and can only run here.
	//
	// A dangling upstream is an ERROR at authoring time even though it is a
	// SUPPORTED runtime state: the prune can create one behind your back and
	// there is nobody to ask, but a repo that names a definition it can see does
	// not exist is a typo, and catching it at MR time is the whole point of
	// Git-authored config.
	//
	// Self-reference is caught here too. Cycles are NOT: a cycle check needs the
	// full cross-plane edge set including edges this sync is about to replace,
	// and getting that wrong could wedge a sync on a graph that is actually fine.
	// The runtime depth ceiling is the backstop, and it is exactly what §2.8 says
	// it is for — a cycle nobody's static check saw.
	// The INCOMING repo is authoritative for git-source definitions, not the DB.
	//
	// Validation runs before the upsert transaction, so checking the database for
	// a git upstream would reject a repo that authors two definitions where one
	// reacts to the other — on the FIRST sync only, then accept it on the second.
	// A repo whose validity depends on how many times it has been synced is not a
	// validation rule, it is a race. Only an `cronomicon`-source upstream (an in-app
	// definition the repo cannot see) is looked up in the DB.
	incomingJobs := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		incomingJobs[j.Metadata.Name] = true
	}
	incomingWfs := make(map[string]bool, len(wfs))
	for _, wf := range wfs {
		incomingWfs[wf.Metadata.Name] = true
	}
	definitionExists := func(kind, source, name string) bool {
		if source == "git" {
			// This repository's own, then Global's (GR-16). Global's are in the
			// tables already: they are not what this sync is writing, so asking
			// the tables for them is not the race the note above describes.
			if kind == "workflow" {
				return incomingWfs[name] || s.globalHas(ctx, kind, name)
			}
			return incomingJobs[name] || s.globalHas(ctx, kind, name)
		}
		table := "jobs"
		if kind == "workflow" {
			table = "workflows"
		}
		var one int
		//nolint:gosec // table is chosen from two literals by the kind switch
		err := s.db.QueryRowContext(ctx,
			"SELECT 1 FROM "+table+" WHERE source = ? AND name = ?", source, name).Scan(&one)
		if err == nil {
			return true
		}
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		// A real DB error is NOT "no such definition". Failing closed here would
		// turn a transient SQLITE_BUSY into a hard validation error that drops the
		// owning definition from the sync entirely — the adjacent calendar guard
		// is fail-open for exactly this reason ("a bad spec must not wedge a
		// sync"). Treat an unreadable database as no opinion.
		s.logError("sync: check reaction upstream — treating as present",
			"kind", kind, "source", source, "name", name, "err", err)
		return true
	}
	badReactions := func(file, ownerKind, ownerName string, rs []ReactionEntry) bool {
		if len(rs) == 0 {
			return false
		}
		norm, shapeErrs := NormalizeReactions(rs)
		bad := false
		for _, ve := range shapeErrs {
			ve.File = file
			allErrs = append(allErrs, ve.Error())
			res.Errors = append(res.Errors, ve)
			bad = true
		}
		for _, e := range norm {
			if e.OnKind == ownerKind && e.OnName == ownerName && e.OnSourceOrDefault() == "git" {
				ve := ValidationError{File: file, Field: "spec.reactions[" + e.Name + "].onName",
					Message: "a definition cannot react to itself"}
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
				bad = true
				continue
			}
			if !definitionExists(e.OnKind, e.OnSourceOrDefault(), e.OnName) {
				ve := ValidationError{File: file, Field: "spec.reactions[" + e.Name + "].onName",
					Message: fmt.Sprintf("no such %s %q (source %s)", e.OnKind, e.OnName, e.OnSourceOrDefault())}
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
				bad = true
			}
		}
		return bad
	}

	// Cross-reference: drop any job whose script_ref names a missing script or whose
	// scheduleRef names a missing schedule, with a hard error. Jobs with an inline
	// body / inline schedule pass straight through.
	goodJobs := make([]JobYAML, 0, len(jobs))
	for _, j := range jobs {
		if j.Spec.ScriptRef != "" {
			_, ok := resolved[j.Spec.ScriptRef]
			if !ok {
				_, ok = borrow.script(j.Spec.ScriptRef)
			}
			if !ok {
				ve := ValidationError{File: "jobs/" + j.Metadata.Name + ".yaml", Field: "spec.script_ref",
					Message: fmt.Sprintf("script_ref %q does not resolve to any script in scripts/%s", j.Spec.ScriptRef, elsewhere)}
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
				continue
			}
		}
		if dangleScheduleRefs("jobs/"+j.Metadata.Name+".yaml", j.Spec.ScheduleRefs) {
			continue
		}
		if dangleCalendars("jobs/"+j.Metadata.Name+".yaml", j.Spec.Schedules) {
			continue
		}
		if badReactions("jobs/"+j.Metadata.Name+".yaml", "job", j.Metadata.Name, j.Spec.Reactions) {
			continue
		}
		goodJobs = append(goodJobs, j)
	}
	// A dropped job (missing script_ref / dangling scheduleRef) is a validation
	// failure — skip the jobs prune so a broken ref can't delete a good row (PP-B2).
	if len(goodJobs) != len(jobs) {
		jobsOK = false
	}

	// Confinement (GR-14): a job in an AGENCY's repository must name a scope
	// that agency owns. A file that does not is not written, and is told so.
	//
	// It is that file's error and no more. It is taken out AFTER jobsOK is
	// settled, so that it does not switch the jobs' prune off: one refused file
	// must not freeze a repository's whole job list. And because the prune still
	// runs, a job that WAS synced and whose file now names another agency's
	// scope is removed rather than left running as it was.
	if kept, refused, cerr := s.confineJobs(ctx, goodJobs, scopes); cerr != nil {
		// What could not be asked is not answered "no": nothing is refused,
		// nothing is pruned, and the sync says why.
		allErrs = append(allErrs, "confinement: "+cerr.Error())
		jobsOK = false
	} else {
		goodJobs = kept
		for _, ve := range refused {
			allErrs = append(allErrs, ve.Error())
			res.Errors = append(res.Errors, ve)
		}
	}

	// Same dangling-scheduleRef guard for workflows.
	goodWfs := make([]WorkflowYAML, 0, len(wfs))
	for _, wf := range wfs {
		if dangleScheduleRefs("workflows/"+wf.Metadata.Name+".yaml", wf.Spec.ScheduleRefs) {
			continue
		}
		if dangleCalendars("workflows/"+wf.Metadata.Name+".yaml", wf.Spec.Schedules) {
			continue
		}
		if badReactions("workflows/"+wf.Metadata.Name+".yaml", "workflow", wf.Metadata.Name, wf.Spec.Reactions) {
			continue
		}
		goodWfs = append(goodWfs, wf)
	}
	if len(goodWfs) != len(wfs) {
		wfsOK = false
	}

	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		res.ErrorMessage = "failed to begin transaction: " + txErr.Error()
		res.FinishedAt = time.Now().UTC()
		_ = s.recordSyncEvent(ctx, triggeredBy, res)
		return res
	}
	defer tx.Rollback()

	nowStr := time.Now().UTC().Format(time.RFC3339)
	var dbErr error

	if uErr := s.upsertScripts(ctx, tx, resolved, nowStr, sha); uErr != nil {
		allErrs = append(allErrs, "upsert scripts: "+uErr.Error())
		dbErr = uErr
	} else {
		res.ScriptsSynced = len(resolved)
	}

	if dbErr == nil {
		if uErr := s.upsertSchedules(ctx, tx, resolvedScheds, nowStr, sha); uErr != nil {
			allErrs = append(allErrs, "upsert schedules: "+uErr.Error())
			dbErr = uErr
		} else {
			res.SchedulesSynced = len(resolvedScheds)
		}
	}

	if dbErr == nil {
		if uErr := s.upsertJobs(ctx, tx, goodJobs, borrow.withScripts(resolved), borrow.withScheds(resolvedScheds), nowStr, sha); uErr != nil {
			allErrs = append(allErrs, "upsert jobs: "+uErr.Error())
			dbErr = uErr
		} else {
			res.JobsSynced = len(goodJobs)
		}
	}

	if dbErr == nil {
		if uErr := s.upsertWorkflows(ctx, tx, goodWfs, borrow.withScheds(resolvedScheds), nowStr, sha); uErr != nil {
			allErrs = append(allErrs, "upsert workflows: "+uErr.Error())
			dbErr = uErr
		} else {
			res.WfsSynced = len(goodWfs)
		}
	}

	if dbErr == nil {
		if uErr := s.pointReactionsAtTheirUpstreams(ctx, tx); uErr != nil {
			allErrs = append(allErrs, "resolve reaction upstreams: "+uErr.Error())
			dbErr = uErr
		}
	}

	if dbErr == nil {
		taken, uErr := s.upsertScopes(ctx, tx, scopes, nowStr, sha)
		if uErr != nil {
			allErrs = append(allErrs, "upsert scopes: "+uErr.Error())
			dbErr = uErr
		} else {
			res.ScopesSynced = len(scopes) - len(taken)
			// An inventory whose name is taken was not synced, and its file is
			// told so (GR-19). It is that file's error and no more: it is not one
			// of this repository's scopes, so the prune has nothing to hold back.
			for _, ve := range taken {
				allErrs = append(allErrs, ve.Error())
				res.Errors = append(res.Errors, ve)
			}
		}
	}

	// GitOps Pruning (Item 1): delete rows no longer in Git. PP-B2 — each entity
	// is pruned ONLY when its subsystem parsed cleanly (xxxOK); a subsystem with
	// any parse/resolve/cross-ref error keeps its stale rows for this cycle rather
	// than wiping the catalog. prune() no-ops once dbErr is set so a real DB error
	// still halts the chain.
	prune := func(label, query string, more ...any) int64 {
		if dbErr != nil {
			return 0
		}
		r, err := tx.ExecContext(ctx, query, append([]any{nowStr}, more...)...)
		if err != nil {
			allErrs = append(allErrs, "prune "+label+": "+err.Error())
			dbErr = err
			return 0
		}
		n, _ := r.RowsAffected()
		return n
	}
	// pruneNoArg is prune for queries that take no nowStr bind (PP-H9's
	// paused_jobs orphan sweep keys on "name not in the current git set", not on
	// synced_at). Same dbErr-gate + allErrs-append contract.
	pruneNoArg := func(label, query string) {
		if dbErr != nil {
			return
		}
		if _, err := tx.ExecContext(ctx, query); err != nil {
			allErrs = append(allErrs, "prune "+label+": "+err.Error())
			dbErr = err
		}
	}

	var prunedJobs, prunedWfs, prunedScripts, prunedScopes int64
	var skipped []string

	// Every prune below removes rows of THIS repository that this pass did not
	// stamp, and no other repository's (GR-13). Until 2.4.0 there was one
	// repository and the statements said `source = 'git'`: a second repository's
	// sync would have deleted every definition the first had supplied.
	//
	// The satellites of a definition (its schedule entries, its pause) are found
	// by the definition's uid, not by its name: two repositories may each have a
	// `nightly`. A satellite row with NO owner uid predates the uid, when the one
	// repository there was is the one that is Global's now, so only Global's sync
	// still clears those by name.
	repoID := s.repo()
	legacyByName := 0
	if repoID == repoid.Global {
		legacyByName = 1
	}

	if jobsOK {
		prune("job schedules", `
			DELETE FROM definition_schedules
			WHERE owner_kind = 'job' AND (
				owner_uid IN (SELECT uid FROM jobs
				               WHERE source = 'git' AND synced_at < ?1 AND source_path LIKE 'jobs/%' AND repo_id = ?2)
				OR (?3 = 1 AND owner_uid IS NULL AND owner_source = 'git' AND owner_name IN (
					SELECT name FROM jobs
					 WHERE source = 'git' AND synced_at < ?1 AND source_path LIKE 'jobs/%' AND repo_id = ?2)))`,
			repoID, legacyByName)
		prunedJobs = prune("jobs", `
			DELETE FROM jobs
			WHERE source = 'git' AND synced_at < ? AND source_path LIKE 'jobs/%' AND repo_id = ?`, repoID)
		// PP-H9: clean paused_jobs orphaned by the prune above so a re-added
		// same-named git job doesn't silently inherit a stale pause. Migration 200's
		// AFTER DELETE trigger already does this per-row; this is order-independent
		// defense-in-depth that survives a future trigger regression. Kept INSIDE
		// the jobsOK gate so a parse-skipped subsystem keeps its valid pauses.
		// By the owner's uid: a pause whose job is gone, whichever repository the
		// job was in. (By name it kept an orphan whenever ANOTHER repository had a
		// job of that name.) A pause with no owner uid is matched by name still.
		pruneNoArg("orphaned job pauses", `
			DELETE FROM paused_jobs
			WHERE owner_kind = 'job' AND source = 'git'
			  AND ((owner_uid IS NOT NULL AND owner_uid NOT IN (SELECT uid FROM jobs WHERE source = 'git'))
			    OR (owner_uid IS NULL AND name NOT IN (SELECT name FROM jobs WHERE source = 'git')))`)
	} else {
		skipped = append(skipped, "jobs")
	}

	if wfsOK {
		prune("workflow schedules", `
			DELETE FROM definition_schedules
			WHERE owner_kind = 'workflow' AND (
				owner_uid IN (SELECT uid FROM workflows
				               WHERE source = 'git' AND synced_at < ?1 AND source_path LIKE 'workflows/%' AND repo_id = ?2)
				OR (?3 = 1 AND owner_uid IS NULL AND owner_source = 'git' AND owner_name IN (
					SELECT name FROM workflows
					 WHERE source = 'git' AND synced_at < ?1 AND source_path LIKE 'workflows/%' AND repo_id = ?2)))`,
			repoID, legacyByName)
		prunedWfs = prune("workflows", `
			DELETE FROM workflows
			WHERE source = 'git' AND synced_at < ? AND source_path LIKE 'workflows/%' AND repo_id = ?`, repoID)
		// PP-H9: workflow analog of the paused_jobs orphan sweep above.
		pruneNoArg("orphaned workflow pauses", `
			DELETE FROM paused_jobs
			WHERE owner_kind = 'workflow' AND source = 'git'
			  AND ((owner_uid IS NOT NULL AND owner_uid NOT IN (SELECT uid FROM workflows WHERE source = 'git'))
			    OR (owner_uid IS NULL AND name NOT IN (SELECT name FROM workflows WHERE source = 'git')))`)
	} else {
		skipped = append(skipped, "workflows")
	}

	if scriptsOK {
		// This repository's scripts only (1290, GR-13). The statement used to ask
		// neither for a Git row nor for a repository, so a second repository's
		// sync deleted every script the first had supplied.
		//
		// Not one that a job of ANOTHER repository is joined to (GR-17,
		// deferred.go): it is kept as it was, and that repository's agency is
		// told, until nothing of another repository's uses it.
		prunedScripts = prune("scripts", `
			DELETE FROM scripts
			WHERE synced_at < ? AND source_path LIKE 'scripts/%' AND repo_id = ?
			  AND NOT `+scriptUsedElsewhere, repoID, repoID)
	} else {
		skipped = append(skipped, "scripts")
	}

	// git-source schedules only (source='cronomicon' rows are operator-authored and
	// untouched — same guard as scopes; A9/§5.6).
	if schedsOK {
		// This repository's schedules only (1300, GR-13), like the scripts
		// above and for the same reason: the column arrived with this phase.
		// Nor one that a definition of another repository takes its timing from
		// (GR-17).
		prune("schedules", `
			DELETE FROM schedules
			WHERE source = 'git' AND synced_at < ? AND repo_id = ?
			  AND NOT `+scheduleUsedElsewhere, repoID, repoID, repoID)
	} else {
		skipped = append(skipped, "schedules")
	}

	if scopesOK {
		// SB-1 — a git scope that is BOUND to runners and still has work waiting
		// under its name is held back from this prune, hosts and all. A run
		// carries its scope by name; delete the row and the run reads as
		// unrestricted, so any runner in its agency may claim it — which is the
		// in-app rename/delete refusal (settings.ErrBoundScopeBusy) reached
		// through a commit instead. It is a deferral, not an override of Git: the
		// scope goes on the first sync after its queue drains, and until then the
		// warning below says which scopes are waiting and on what.
		if held := heldBoundScopes(ctx, tx, nowStr, repoID); len(held) > 0 {
			s.logWarn("git sync: scope removed from Git but kept this cycle — it is bound to runners and has runs "+
				"waiting under its name; it will be pruned once they finish or are cancelled",
				"scopes", strings.Join(held, ", "))
		}
		prune("scope hosts", `
			DELETE FROM scope_hosts
			WHERE scope_id IN (
				SELECT id FROM scopes WHERE synced_at < ? AND source = 'git' AND repo_id = ?
				   AND NOT `+settings.BoundScopeBusySQL+`
			)`, repoID)
		// M4 — reap imported (git) ssh_hosts rows dropped from inventory this sync.
		// Only source='git' rows; operator (cronomicon) overlays are never touched.
		//
		// The records of THIS repository's scopes, and not of a scope that is being
		// held back above: a held scope is kept "hosts and all", and until 2.4.0
		// this statement asked neither which scope a record belonged to nor whether
		// that scope was held, so a held scope kept its host membership and lost
		// the records its hosts are reached by (present defect 8). A record that
		// belongs to no scope at all goes whichever repository is syncing.
		prune("imported ssh hosts", `
			DELETE FROM ssh_hosts
			WHERE source = 'git' AND (synced_at IS NULL OR synced_at < ?1)
			  AND (scope_id IS NULL
			       OR scope_id NOT IN (SELECT id FROM scopes)
			       OR scope_id IN (SELECT id FROM scopes
			                        WHERE source = 'git' AND repo_id = ?2
			                          AND NOT (synced_at < ?1 AND `+settings.BoundScopeBusySQL+`)))`, repoID)
		prunedScopes = prune("scopes", `
			DELETE FROM scopes
			WHERE synced_at < ? AND source = 'git' AND repo_id = ?
			  AND NOT `+settings.BoundScopeBusySQL, repoID)
	} else {
		skipped = append(skipped, "scopes")
	}

	// The prunes this sync deferred, said to whoever they wait for (GR-17).
	if dbErr == nil {
		if err := s.noteDeferredPrunes(ctx, tx, nowStr, scriptsOK, schedsOK); err != nil {
			allErrs = append(allErrs, "note the deferred prunes: "+err.Error())
			dbErr = err
		}
	}

	// PP-B2 (B2-3): a skipped prune leaves stale rows — surface it. The parse
	// errors are already in allErrs (status → "partial"); this names the affected
	// subsystems for the operator.
	if len(skipped) > 0 {
		s.logWarn("git sync: prune skipped for subsystems with parse errors — stale rows retained this cycle",
			"subsystems", strings.Join(skipped, ", "))
	}
	// PP-B2 (B2-4): a full wipe of a kind (rows deleted while zero synced) is the
	// signature of an accidental empty/rewritten tree — log it loudly even when the
	// parse looked clean, so an operator can catch a bad force-push.
	if (prunedJobs > 0 && res.JobsSynced == 0) || (prunedWfs > 0 && res.WfsSynced == 0) ||
		(prunedScripts > 0 && res.ScriptsSynced == 0) || (prunedScopes > 0 && res.ScopesSynced == 0) {
		s.logWarn("git sync: a definition kind was fully pruned (0 synced, rows deleted) — verify the repo was not mid-rewrite",
			"jobs_pruned", prunedJobs, "workflows_pruned", prunedWfs,
			"scripts_pruned", prunedScripts, "scopes_pruned", prunedScopes)
	}

	if dbErr == nil {
		// What this sync saw, on the repository's own row (was the singleton
		// git_sync_state until migration 1310). An UPDATE: a repository that is
		// being synced has a row, and a sync never creates one.
		var stateRes sql.Result
		stateRes, dbErr = tx.ExecContext(ctx,
			`UPDATE git_repos SET last_sha=?, last_synced_at=?, last_status=? WHERE id=?`,
			sha, nowStr, func() string {
				if len(allErrs) == 0 {
					return "success"
				}
				return "partial"
			}(), s.repo())
		if dbErr != nil {
			allErrs = append(allErrs, "update sync state: "+dbErr.Error())
		} else if n, _ := stateRes.RowsAffected(); n == 0 {
			// The definitions are written and the commit is in the history row;
			// only "what the last sync saw" has nowhere to go. Said, not failed.
			s.logWarn("git sync: the repository has no row to record its sync state on", "repo_id", s.repo())
		}
	}

	// The definitions generation (GR-29, migration 1330): one more, in this
	// transaction, whichever repository this is. It is what the scheduler
	// reloads on. It was the commit of the one repository; with a repository per
	// agency there is no one commit, and a sync of an agency's repository at an
	// unchanged commit of Global's must still be seen.
	var generation int64
	if dbErr == nil {
		dbErr = tx.QueryRowContext(ctx, `
			INSERT INTO definitions_generation (id, n) VALUES (1, 1)
			ON CONFLICT(id) DO UPDATE SET n = n + 1
			RETURNING n`).Scan(&generation)
		if dbErr != nil {
			allErrs = append(allErrs, "advance the definitions generation: "+dbErr.Error())
		}
	}

	// The repository's problems as of this sync (GR-30), in this transaction:
	// every file error it reported and everything it warned of. If the
	// transaction does not commit, the rows of the last sync that did stay.
	if dbErr == nil {
		s.noteErrors(res.Errors)
		if dbErr = s.writeProblems(ctx, tx, nowStr, s.currentProblems()); dbErr != nil {
			allErrs = append(allErrs, "record the sync's problems: "+dbErr.Error())
		}
	}

	if dbErr == nil {
		if commitErr := tx.Commit(); commitErr != nil {
			allErrs = append(allErrs, "commit transaction: "+commitErr.Error())
			dbErr = commitErr
		}
	}

	res.FinishedAt = time.Now().UTC()
	if dbErr == nil && len(allErrs) == 0 {
		res.Status = "success"
	} else {
		res.Status = "partial"
		res.ErrorMessage = strings.Join(allErrs, "; ")
	}

	_ = s.recordSyncEvent(ctx, triggeredBy, res)

	// Notify the scheduler so freshly-synced schedules take effect at once. The
	// callback compares the generation with the one it loaded at.
	//
	// Only when the sync COMMITTED, and on a context that cannot end. The
	// scheduler's reload removes every cron entry and then reads them back; run
	// on a cancelled context it removes them and reads nothing, and since a
	// rolled-back sync changed neither the commit nor the tables, nothing tells
	// it to try again: every schedule stops firing. A sync is cancelled whenever
	// its Service is stopped (a connection was saved, GR-11) or its caller goes
	// away (a scope resync), and its transaction is where the cancellation lands.
	if s.onSyncComplete != nil && dbErr == nil {
		s.onSyncComplete(context.WithoutCancel(ctx), strconv.FormatInt(generation, 10))
	}
	return res
}

// writeBranch resolves the working branch used for both sync (read) and publish
// (write): env override (Global's repository only, GR-21) → the repository's
// row (git_repos.branch) → "main". Read fresh per operation so a settings
// change takes effect without a restart (V1.1-10).
func (s *Service) writeBranch(ctx context.Context) string {
	if b := settings.EnvBranch(s.Cfg, s.repo()); b != "" {
		return b
	}
	if s.db != nil {
		var b sql.NullString
		if err := s.db.QueryRowContext(ctx,
			`SELECT branch FROM git_repos WHERE id=?`, s.repo()).Scan(&b); err == nil && b.Valid && b.String != "" {
			return b.String
		}
	}
	return "main"
}

// cloneOrFetch clones the repo if not present; otherwise fetches and hard-resets.
// After either path it initializes/updates any git submodules (best-effort) so
// files contributed by a submodule — e.g. an Ansible playbooks repo mounted at
// scripts/playbooks — are present in the working tree for script discovery.
//
// An existing clone is made to follow the CONNECTION before it is fetched
// (GR-12): its `origin` is set to the configured URL and to fetch the
// configured branch. Until 2.4.0 the URL was used once, to clone, and the
// clone was single-branch, so a changed URL was never applied (the server went
// on syncing the old repository) and a changed branch was never fetched (every
// sync after it failed).
func (s *Service) cloneOrFetch(ctx context.Context, branch string) (*gogit.Repository, error) {
	if s.repoURL == "" {
		return nil, fmt.Errorf("CRONOMICON_GITLAB_BASE_URL not configured")
	}
	s.installGuardedGitTransport() // SU-7/SU-8: guard go-git http(s) egress (once)

	auth := s.gitAuth()

	// Try to open existing clone.
	repo, err := gogit.PlainOpen(s.cloneDir)
	if err == nil {
		wt, err := repo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("worktree: %w", err)
		}
		refSpec := gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch))
		if err := s.pointOriginAt(repo, refSpec); err != nil {
			return nil, err
		}
		fetchOpts := &gogit.FetchOptions{RemoteName: "origin", Force: true, RefSpecs: []gitconfig.RefSpec{refSpec}}
		if auth != nil {
			fetchOpts.Auth = auth
		}
		ferr := repo.FetchContext(ctx, fetchOpts)
		if ferr != nil && ferr != gogit.NoErrAlreadyUpToDate {
			return nil, fmt.Errorf("git fetch: %w", ferr)
		}
		// Hard-reset to origin/<branch>.
		remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
		if err != nil {
			return nil, fmt.Errorf("resolve origin/%s: %w", branch, err)
		}
		// The local branch of that name, at that commit, with HEAD on it: what
		// `git checkout -B <branch> origin/<branch>` does. On a clone made at
		// this branch it changes nothing (the reset below moves the same ref).
		// After the connection's branch has changed, it is what gives publish a
		// local branch of the new name to commit on and push.
		local := plumbing.NewBranchReferenceName(branch)
		if err := repo.Storer.SetReference(plumbing.NewHashReference(local, remoteRef.Hash())); err != nil {
			return nil, fmt.Errorf("set %s: %w", local, err)
		}
		if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, local)); err != nil {
			return nil, fmt.Errorf("point HEAD at %s: %w", local, err)
		}
		if err := wt.Reset(&gogit.ResetOptions{
			Commit: remoteRef.Hash(),
			Mode:   gogit.HardReset,
		}); err != nil {
			return nil, fmt.Errorf("git reset --hard: %w", err)
		}
		// Pull any submodules up to their pinned commits so their files are
		// present for discovery (e.g. scripts/playbooks). Best-effort.
		s.updateSubmodules(ctx, repo, wt, auth)
		return repo, nil
	}

	// Clone fresh.
	if err := os.MkdirAll(s.cloneDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir clone dir: %w", err)
	}
	cloneOpts := &gogit.CloneOptions{
		URL:           s.repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	}
	if auth != nil {
		cloneOpts.Auth = auth
	}
	repo, err = gogit.PlainCloneContext(ctx, s.cloneDir, false, cloneOpts)
	if err != nil {
		return nil, fmt.Errorf("git clone %s: %w", s.repoURL, err)
	}
	// Initialize + check out submodules so their files are present for discovery.
	// RecurseSubmodules is intentionally NOT set on CloneOptions above so that a
	// submodule fetch failure cannot fail the whole clone — updateSubmodules runs
	// it as a separate best-effort step instead.
	if wt, werr := repo.Worktree(); werr == nil {
		s.updateSubmodules(ctx, repo, wt, auth)
	}
	return repo, nil
}

// pointOriginAt makes the clone's `origin` name the configured URL and fetch
// the configured branch, writing the clone's config only when it says
// otherwise (which is: never, on a clone whose connection has not changed).
func (s *Service) pointOriginAt(repo *gogit.Repository, refSpec gitconfig.RefSpec) error {
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("read the clone's config: %w", err)
	}
	origin := cfg.Remotes["origin"]
	if origin != nil && len(origin.URLs) == 1 && origin.URLs[0] == s.repoURL &&
		len(origin.Fetch) == 1 && origin.Fetch[0] == refSpec {
		return nil
	}
	if origin != nil && len(origin.URLs) > 0 && origin.URLs[0] != s.repoURL {
		// Not the URL: it may carry a credential. That it changed is the news.
		s.logInfo("git sync: the repository's URL has changed; the clone now fetches from the new one", "repo_id", s.repo())
	}
	cfg.Remotes["origin"] = &gitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{s.repoURL},
		Fetch: []gitconfig.RefSpec{refSpec},
	}
	if err := repo.SetConfig(cfg); err != nil {
		return fmt.Errorf("point the clone's origin at the configured repository: %w", err)
	}
	return nil
}

func (s *Service) gitAuth() *gogithttp.BasicAuth {
	if s.token == "" {
		return nil
	}
	return &gogithttp.BasicAuth{Username: "oauth2", Password: s.token}
}

// updateSubmodules initializes and checks out every git submodule (recursively)
// in the worktree so files they contribute are present for discovery — most
// importantly an Ansible playbooks repo mounted under scripts/ (its playbooks
// only surface on the Scripts page once their files exist on disk). It is
// deliberately best-effort: each submodule is updated independently and any
// failure (an SSH-URL submodule, or a repo the sync token can't read) is logged
// and skipped rather than failing the whole sync, so the superproject's own
// files always load. Submodules are fetched with the same token as the parent
// repo via SubmoduleUpdateOptions.Auth; a repo with no .gitmodules yields an
// empty list and is a no-op.
//
// Only for Global's repository. A submodule's URL is whatever .gitmodules
// says, which is whatever a committer wrote, and it is fetched with the
// repository's token. Global's committers are the installation's
// administrators. An agency's are not, and the URL its connection may name is
// held to an allowlist (GR-10) that a submodule's URL would walk around: an
// agency's repository's submodules are not fetched, and the sync says so.
func (s *Service) updateSubmodules(ctx context.Context, repo *gogit.Repository, wt *gogit.Worktree, auth *gogithttp.BasicAuth) {
	if s.repo() != repoid.Global {
		if f, err := wt.Filesystem.Open(".gitmodules"); err == nil {
			_ = f.Close()
			s.logWarn("git sync: this repository declares submodules, which are fetched for the installation's own "+
				"repository only; their files are not present and nothing in them is synced",
				"file", ".gitmodules", "repo_id", s.repo())
		}
		return
	}
	// Sync submodule URLs from .gitmodules to .git/config (equivalent to git submodule sync).
	// Since go-git does not sync them automatically on URL change, we manually synchronize
	// the configured URL from .gitmodules.
	if gitmodulesFile, err := wt.Filesystem.Open(".gitmodules"); err == nil {
		defer gitmodulesFile.Close()
		if data, err := io.ReadAll(gitmodulesFile); err == nil {
			gitmodulesCfg := gitconfig.NewConfig()
			if err := gitmodulesCfg.Unmarshal(data); err == nil {
				if repoCfg, err := repo.Config(); err == nil {
					dirty := false
					for _, subCfg := range gitmodulesCfg.Submodules {
						if parentSub, ok := repoCfg.Submodules[subCfg.Name]; ok {
							if parentSub.URL != subCfg.URL {
								s.logInfo("syncing submodule URL", "submodule", subCfg.Name, "old", parentSub.URL, "new", subCfg.URL)
								parentSub.URL = subCfg.URL
								dirty = true
							}
						}
					}
					if dirty {
						if err := repo.SetConfig(repoCfg); err != nil {
							s.logWarn("failed to write synced submodule config", "err", err)
						}
					}
				}
			}
		}
	}

	subs, err := wt.Submodules()
	if err != nil {
		s.logWarn("list submodules failed; submodule files skipped this sync", "err", err)
		return
	}
	declared := make(map[string]bool, len(subs))
	opts := &gogit.SubmoduleUpdateOptions{
		Init:              true,
		RecurseSubmodules: gogit.DefaultSubmoduleRecursionDepth,
	}
	if auth != nil {
		opts.Auth = auth
	}
	for _, sub := range subs {
		cfg := sub.Config()
		declared[cfg.Path] = true
		// With the sync's context: a submodule fetch is a transfer like the
		// repository's own, and a Service that is stopped must not have to wait
		// for one that has stalled.
		if err := sub.UpdateContext(ctx, opts); err != nil {
			s.logWarn("update submodule failed; its files will be skipped this sync",
				"submodule", cfg.Name, "path", cfg.Path, "err", err)
			continue
		}
		s.logInfo("submodule updated", "submodule", cfg.Name, "path", cfg.Path)
	}
	s.warnUndeclaredGitlinks(repo, declared)
}

// warnUndeclaredGitlinks scans the HEAD tree for submodule gitlinks (mode
// 0160000) that have no matching entry in .gitmodules. Such a gitlink records a
// pinned commit but no URL, so neither git nor go-git can fetch its files — the
// directory checks out empty and any scripts under it silently never appear.
// (This is the classic result of `git add`-ing a nested clone instead of
// `git submodule add`-ing it.) We surface it loudly with the path and pinned
// commit so the fix — add a [submodule] mapping with the repo URL to
// .gitmodules — is obvious. Diagnostic only; it changes nothing on disk.
func (s *Service) warnUndeclaredGitlinks(repo *gogit.Repository, declared map[string]bool) {
	head, err := repo.Head()
	if err != nil {
		return
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return
	}
	tree, err := commit.Tree()
	if err != nil {
		return
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		name, entry, err := w.Next()
		if err != nil {
			break // io.EOF at the end of the tree
		}
		if entry.Mode == filemode.Submodule && !declared[name] {
			s.logWarn("submodule gitlink has no .gitmodules entry; its files cannot be fetched — add a [submodule] mapping with the repo URL to .gitmodules",
				"path", name, "pinned_commit", entry.Hash.String())
		}
	}
}

func (s *Service) headSHA(repo *gogit.Repository) (string, error) {
	ref, err := repo.Head()
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Parse jobs/*.yaml
// ──────────────────────────────────────────────────────────────────────────────

// manifestFile is one discovered definition file: its path relative to the
// category dir (slash-normalized, e.g. "db/backup.yaml") plus the absolute path
// (for error labels) and bytes.
type manifestFile struct {
	rel  string
	path string
	data []byte
}

// fileReader reads one file by its path.
type fileReader func(path string) ([]byte, error)

// errLeavesRepository is what the contained reader answers for a file that is
// not inside the repository once its symbolic links are followed.
var errLeavesRepository = errors.New("is a symbolic link that leads out of the repository; not read")

// errLinkNotFollowed is what it answers for a link whose target is not there.
var errLinkNotFollowed = errors.New("is a symbolic link that cannot be followed; not read")

// containedReader reads files of a clone, and only of that clone: a path
// that, with its symbolic links followed, is outside root is refused.
//
// A repository can hold a symbolic link, and a link can point anywhere. Until
// 2.4.0 only a script's BODY was read this way (execspec.SafeReadRepoFile);
// job, schedule, workflow and inventory files were read with os.ReadFile, so a
// committed link named `inventory/x.ini` was read from wherever it pointed,
// and its content stored as a scope's inventory or quoted in a parse error.
// With one repository whose committers are the installation's administrators
// that reads nothing they could not read anyway. With a clone per agency side
// by side in one directory (GR-12), it is one agency's committer reading
// another agency's repository.
//
// A link that stays inside the repository is followed, as before.
func containedReader(root string) fileReader {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	return func(path string) ([]byte, error) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			// A link that cannot be followed. The error would name where it
			// points, which is not this repository's reader's to be told.
			if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
				return nil, errLinkNotFollowed
			}
			return nil, err
		}
		rel, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
			return nil, errLeavesRepository
		}
		// Nor the clone's own .git: it is inside the directory and is no part of
		// what the repository holds. Its config names the connection's URL, and a
		// link `inventory/x.ini -> ../.git/config` would store that as a scope's
		// inventory.
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			return nil, errLeavesRepository
		}
		return os.ReadFile(resolved)
	}
}

// readCloned reads one file of this Service's clone through the contained
// reader. Everything a sync reads from the clone comes through here or
// through execspec.SafeReadRepoFile.
func (s *Service) readCloned(path string) ([]byte, error) {
	return containedReader(s.cloneDir)(path)
}

// readClonedDir says where a directory of the clone really is, refusing one
// that, with its links followed, is outside the clone or in its .git.
func (s *Service) readClonedDir(dir string) (string, error) {
	root, err := filepath.EvalSymlinks(s.cloneDir)
	if err != nil {
		root = s.cloneDir
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errLinkNotFollowed
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) ||
		rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
		return "", errLeavesRepository
	}
	return resolved, nil
}

// walkManifests recursively collects every .yaml/.yml file under dir so jobs,
// workflows, and schedules can live in sub-folders (folder support). A missing
// dir is not an error; a read failure is recorded but never aborts the walk,
// mirroring discoverScripts' advisory model.
func walkManifests(dir string, read fileReader) ([]manifestFile, []error) {
	var out []manifestFile
	var errs []error
	werr := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			if os.IsNotExist(e) {
				return nil
			}
			return e
		}
		if d.IsDir() {
			return nil
		}
		n := d.Name()
		if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
			return nil
		}
		data, rerr := read(p)
		if rerr != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", p, rerr))
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, manifestFile{rel: filepath.ToSlash(rel), path: p, data: data})
		return nil
	})
	if werr != nil && !os.IsNotExist(werr) {
		errs = append(errs, fmt.Errorf("walk %s: %w", dir, werr))
	}
	return out, errs
}

func (s *Service) parseJobs() ([]JobYAML, []error) {
	dir := filepath.Join(s.cloneDir, "jobs")
	files, errs := walkManifests(dir, s.readCloned)
	var jobs []JobYAML
	for _, f := range files {
		// Validate apiVersion/kind first.
		valErrs, _ := validateYAMLBytes(f.path, f.data)
		if len(valErrs) > 0 {
			for _, ve := range valErrs {
				errs = append(errs, ve)
			}
			continue
		}
		var j JobYAML
		if err := yaml.Unmarshal(f.data, &j); err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", f.path, err))
			continue
		}
		j.Spec.ConcurrencyPolicy = normalizeConcurrencyPolicy(j.Spec.ConcurrencyPolicy)
		j.SourcePath = "jobs/" + f.rel
		jobs = append(jobs, j)
	}
	return jobs, errs
}

// normalizeConcurrencyPolicy degrades an unknown value to Allow (JR-Q5's rule:
// a typo in Git must not take a job offline). It delegates so the sync and
// compose paths cannot disagree about the vocabulary — they did, silently, for
// as long as `Replace` was accepted by both and honoured by neither.
func normalizeConcurrencyPolicy(p string) string { return cronutil.NormalizePolicy(p) }

// ──────────────────────────────────────────────────────────────────────────────
// Parse scripts/*.yaml (B-Git)
// ──────────────────────────────────────────────────────────────────────────────

func (s *Service) parseScripts() ([]ScriptYAML, []error) {
	dir := filepath.Join(s.cloneDir, "scripts")
	return discoverScriptsWith(dir, s.readCloned)
}

// ──────────────────────────────────────────────────────────────────────────────
// Parse schedules/*.yaml (A10a — first-class Schedules)
// ──────────────────────────────────────────────────────────────────────────────

func (s *Service) parseSchedules() ([]ScheduleYAML, []error) {
	dir := filepath.Join(s.cloneDir, "schedules")
	files, errs := walkManifests(dir, s.readCloned)
	var scheds []ScheduleYAML
	for _, f := range files {
		valErrs, _ := validateYAMLBytes(f.path, f.data)
		if len(valErrs) > 0 {
			for _, ve := range valErrs {
				errs = append(errs, ve)
			}
			continue
		}
		var sd ScheduleYAML
		if err := yaml.Unmarshal(f.data, &sd); err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", f.path, err))
			continue
		}
		sd.SourcePath = "schedules/" + f.rel
		scheds = append(scheds, sd)
	}
	return scheds, errs
}

// ──────────────────────────────────────────────────────────────────────────────
// Parse workflows/*.yaml
// ──────────────────────────────────────────────────────────────────────────────

func (s *Service) parseWorkflows() ([]WorkflowYAML, []error) {
	dir := filepath.Join(s.cloneDir, "workflows")
	files, errs := walkManifests(dir, s.readCloned)
	var wfs []WorkflowYAML
	for _, f := range files {
		valErrs, _ := validateYAMLBytes(f.path, f.data)
		if len(valErrs) > 0 {
			for _, ve := range valErrs {
				errs = append(errs, ve)
			}
			continue
		}
		var wf WorkflowYAML
		if err := yaml.Unmarshal(f.data, &wf); err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", f.path, err))
			continue
		}
		wf.SourcePath = "workflows/" + f.rel
		wfs = append(wfs, wf)
	}
	return wfs, errs
}

// ──────────────────────────────────────────────────────────────────────────────
// Parse inventory/*.ini (+ optional *.cronomicon.yaml sidecars)
// ──────────────────────────────────────────────────────────────────────────────

// inventoryScope is the parsed representation of a single inventory file.
type inventoryScope struct {
	Name       string
	SourcePath string
	Content    string // byte-exact raw inventory (the -i material; persisted to scopes.raw_inventory)
	Format     string // "ini" (only .ini is parsed today)
	// Meta is the Git-declared metadata (owner, sidecar path, pragma errors)
	// persisted to scopes.git_meta_json. Before 2.1.0 this was the scope's
	// run-type "capability"; the run types were removed (migration 1170).
	Meta        InventoryMeta
	SidecarPath string
	Hosts       []string // EX.5 — parsed inventory host membership (for in-app SSH targeting)
	Description string
	// Projection is the M2 advisory parsed view (groups, host_vars, degrade flags)
	// written to the scope_* projection tables + scopes.projection_status.
	Projection inventory.Projection
	// AuthKeyEnvVar is the per-scope sidecar default auth-key env-var NAME (M4 /
	// §9.2) used when importing this inventory's hosts into ssh_hosts.
	AuthKeyEnvVar string
}

// parseInventoryHosts extracts host names from an Ansible-style .ini inventory.
// It delegates to inventory.Hosts so the host-membership rule has ONE
// implementation shared with the projection parser (the two can't drift).
func parseInventoryHosts(content string) []string {
	return inventory.Hosts(content)
}

func (s *Service) parseInventories() ([]inventoryScope, []error) {
	dir := filepath.Join(s.cloneDir, "inventory")
	// `inventory` itself may be a link. os.ReadDir follows it, so one that led
	// out of the repository would be LISTED here, and each name in it reported
	// by the refusal of the file: the names of another repository's scopes.
	// (The walkers of the other kinds do not follow a linked directory.)
	if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if _, cerr := s.readClonedDir(dir); cerr != nil {
			return nil, []error{ValidationError{File: "inventory", Message: cerr.Error()}}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("read inventory dir: %w", err)}
	}

	// Index sidecars by base name.
	sidecars := map[string]*sidecarYAML{}
	sidecarPaths := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cronomicon.yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := s.readCloned(path)
		if err != nil {
			continue
		}
		var sc sidecarYAML
		if err := yaml.Unmarshal(data, &sc); err != nil {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".cronomicon.yaml")
		sidecars[base] = &sc
		sidecarPaths[base] = "inventory/" + e.Name()
	}

	var scopes []inventoryScope
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ini") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := s.readCloned(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", "inventory/"+e.Name(), err))
			continue
		}
		content := string(data)

		// Path A (D1): reject secret-bearing inventory at INGEST. A reject SKIPS
		// the whole scope (fail-closed) so no secret value is ever persisted or
		// shipped to a runner. This is the single load-bearing guard keeping
		// decrypted secrets off the runner path; a bad file also suppresses scope
		// pruning for this sync (scopesOK) — the intended safe failure mode.
		if secErrs := inventory.ValidateSecrets(content, "inventory/"+e.Name()); len(secErrs) > 0 {
			for _, se := range secErrs {
				errs = append(errs, se)
			}
			continue
		}

		base := strings.TrimSuffix(e.Name(), ".ini")
		sidecar := sidecars[base]
		sidecarPath := sidecarPaths[base]

		meta := resolveInventoryMeta(content, sidecar, sidecarPath)
		for _, me := range meta.Errors {
			errs = append(errs, me)
		}
		// ST-Q1: warnings are logged and go NO further. Appending one to errs
		// would mark the sync `partial` and suppress scope pruning (scopesOK) for
		// every repository that still declares the retired `types` directive.
		for _, w := range meta.Warnings {
			s.logWarn("inventory pragma: "+w.Message, "file", "inventory/"+e.Name(), "line", w.Line)
		}

		scopes = append(scopes, inventoryScope{
			Name:          base,
			SourcePath:    "inventory/" + e.Name(),
			Content:       content,
			Format:        "ini",
			Meta:          meta,
			SidecarPath:   sidecarPath,
			Hosts:         parseInventoryHosts(content),
			Description:   meta.Description,
			Projection:    inventory.ParseProjection(content),
			AuthKeyEnvVar: sidecarAuthKeyEnvVar(sidecar),
		})
	}
	return scopes, errs
}

// ──────────────────────────────────────────────────────────────────────────────
// DB upserts
// ──────────────────────────────────────────────────────────────────────────────

// resolvedScript is a Script with its content hash computed (scriptPath files
// read from the clone). It feeds both the scripts-table upsert and the
// denormalization of fields onto referencing jobs (Decision 7).
type resolvedScript struct {
	// uid is the script's identity in the scripts table (migration 1290). It is
	// empty until upsertScripts has written the row, which fills it in: a row
	// that already existed keeps its uid, a new one is given one. upsertJobs
	// reads it to stamp jobs.script_uid.
	uid         string
	runType     string
	command     string
	script      string
	scriptPath  string
	executor    string // what is STORED: storableExecutor of the sidecar's value
	description string
	contentHash string
	sourcePath  string
	projectRoot string // §7 checkout project: repo-relative dir, "" for a plain script
	warnings    string // JSON array of body-lint Warnings ("[]" when clean)
	variables   string // JSON array of referenced env Variables ("[]" when none)
	prompts     string // JR-Q6 — JSON array of DECLARED run inputs ("[]" when none)
}

// storableExecutor is what a retired `spec.executor` value becomes on its way
// to `jobs.executor` / `scripts.executor` (2.3.0, LR-48). The key is read by
// nothing, and validation no longer refuses a value it never had — but both
// columns still carry CHECK (executor IN ('runner','ssh')), so a value outside
// it would fail the INSERT and, with it, the whole sync. The two values the
// key did have are kept, because the 2.3.0 upgrade pass and the
// leftover_executor_key notice read them; anything else is stored as nothing
// and reported by the warnings alone.
func storableExecutor(v string) string {
	switch v = strings.TrimSpace(v); v {
	case execspec.ExecutorSSH, execspec.ExecutorRunner:
		return v
	}
	return ""
}

// resolveBodyHash computes the Decision-8 content hash for an executable body
// resolved against the clone (see body.go). Inline command/
// script hash the string; scriptPath reads the file (validated repo-relative by
// safeScriptPath at parse time) and hashes its bytes.
func (s *Service) resolveBodyHash(command, script, scriptPath string) (string, error) {
	return hashBodyAt(s.cloneDir, command, script, scriptPath)
}

// resolveBody reads an executable body's raw bytes against the clone (shared with
// body.go's readBodyAt), so resolveScripts can hash AND body-lint the identical
// bytes in one read.
func (s *Service) resolveBody(command, script, scriptPath string) ([]byte, error) {
	return readBodyAt(s.cloneDir, command, script, scriptPath)
}

// resolveScripts computes each script's content hash, returning a name→resolved
// map plus any errors (e.g. an unreadable scriptPath). A script that fails to
// resolve is omitted from the map so its referencing jobs surface as dangling.
func (s *Service) resolveScripts(scripts []ScriptYAML) (map[string]resolvedScript, []error) {
	out := make(map[string]resolvedScript, len(scripts))
	var errs []error
	for _, sc := range scripts {
		name := sc.Metadata.Name
		if name == "" {
			errs = append(errs, ValidationError{Field: "metadata.name", Message: "script is missing metadata.name"})
			continue
		}
		// Read the body once, then hash AND body-lint the identical bytes (so the
		// scan can never disagree with the hash about what the file contains). A
		// scan is advisory: it never errors and a finding never drops the script.
		raw, err := s.resolveBody(sc.Spec.Command, sc.Spec.Script, sc.Spec.ScriptPath)
		if err != nil {
			// Not the reader's own text when the body is a file: it names where a
			// link resolves to, which tells whoever reads the sync's problems what
			// is on the server's disk (a file that exists and one that does not
			// answer differently).
			msg := "compute content hash: " + err.Error()
			if strings.TrimSpace(sc.Spec.ScriptPath) != "" {
				msg = fmt.Sprintf("compute content hash: the script's file %q could not be read: "+
					"it is missing, or is a link that leads out of the repository", sc.Spec.ScriptPath)
			}
			errs = append(errs, ValidationError{File: sc.SourcePath, Message: msg})
			continue
		}
		// LR-48 — a sidecar's `executor` is retired with the job's. Said here
		// for the script itself: one that only composed jobs reference, or none,
		// would otherwise be named by no job's warning.
		if v := strings.TrimSpace(sc.Spec.Executor); v != "" {
			s.logWarn("git sync: script still declares executor, which is no longer read; remove the line",
				"script", name, "source_path", sc.SourcePath, "executor", v)
		}
		out[name] = resolvedScript{
			runType:     sc.Spec.RunType,
			command:     sc.Spec.Command,
			script:      sc.Spec.Script,
			scriptPath:  sc.Spec.ScriptPath,
			executor:    storableExecutor(sc.Spec.Executor),
			description: sc.Spec.Description,
			contentHash: ContentHash(raw),
			sourcePath:  sc.SourcePath,
			projectRoot: sc.Spec.ProjectRoot,
			warnings:    marshalWarnings(ScanScriptBody(raw, sc.Spec.RunType)),
			variables:   marshalVariables(ExtractScriptVariables(raw, sc.Spec.RunType)),
			// JR-Q6 — declared run inputs; shares MarshalPrompts with the jobs path so
			// the two can't drift in serialization (nameless rows dropped, "[]" when empty).
			prompts: MarshalPrompts(sc.Spec.Prompts),
		}
	}
	return out, errs
}

// upsertScripts writes the resolved scripts into the scripts cache table, and
// records each row's uid back into resolved (see resolvedScript.uid).
//
// A script is unique by (repo_id, name) since migration 1290, and its uid is
// its identity: an edit in Git updates the row and keeps the uid, so the
// reference bindings filed under it and the jobs joined to it stay put. The uid
// is therefore ABSENT from the DO UPDATE below, like tags. A script that was
// pruned and comes back is a new row with a new uid, and its bindings went
// with the old one (the cleanup trigger).
//
// NOTE: scripts.tags (migration 280) is deliberately ABSENT from both the column
// list and the ON CONFLICT DO UPDATE SET below. Unlike warnings/variables (which
// are recomputed from the Git body every sync), tags are user-authored
// (PUT /script-tags) and must survive syncs: the column DEFAULT '[]' covers a
// freshly-inserted row and omission from DO UPDATE preserves an existing row's
// tags. Do NOT add tags here — TestScriptTagsSurviveSync guards this.
func (s *Service) upsertScripts(ctx context.Context, tx *sql.Tx, resolved map[string]resolvedScript, now string, sha string) error {
	for name, rs := range resolved {
		warnings := rs.warnings
		if warnings == "" {
			warnings = "[]"
		}
		variables := rs.variables
		if variables == "" {
			variables = "[]"
		}
		prompts := rs.prompts
		if prompts == "" {
			prompts = "[]"
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO scripts(uid, repo_id, name, description, run_type, command, script, script_path,
			                    executor, content_hash, source_path, synced_at, warnings, variables,
			                    project_root, prompts_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(repo_id, name) DO UPDATE SET
				description=excluded.description,
				run_type=excluded.run_type,
				command=excluded.command,
				script=excluded.script,
				script_path=excluded.script_path,
				executor=excluded.executor,
				content_hash=excluded.content_hash,
				source_path=excluded.source_path,
				synced_at=excluded.synced_at,
				warnings=excluded.warnings,
				variables=excluded.variables,
				project_root=excluded.project_root,
				prompts_json=excluded.prompts_json`,
			db.NewID(), s.repo(), name, nullStr(rs.description), rs.runType,
			nullStr(rs.command), nullStr(rs.script), nullStr(rs.scriptPath),
			nullStr(rs.executor), rs.contentHash, nullStr(rs.sourcePath), now, warnings, variables,
			nullStr(rs.projectRoot), prompts)
		if err != nil {
			return fmt.Errorf("upsert script %q: %w", name, err)
		}
		// The blind upsert cannot say whether it inserted or updated, so the uid
		// is read back: the minted one for a new script, the standing one for a
		// script that was already there.
		if err := tx.QueryRowContext(ctx,
			`SELECT uid FROM scripts WHERE repo_id = ? AND name = ?`, s.repo(), name).Scan(&rs.uid); err != nil {
			return fmt.Errorf("resolve uid for script %q: %w", name, err)
		}
		resolved[name] = rs
		// An in-app job names its script and holds a copy of it. If the script
		// had gone (the trigger cleared the job's script_uid) and is back, the
		// job is joined to it again by the name its author wrote, which is what
		// the name did by itself while a script WAS its name.
		//
		// Which repository an in-app job's name is looked up in is GR-16's rule:
		// its agency's repository, then Global's. So an agency's repository joins
		// the in-app jobs of ITS agency, and Global's joins every other, leaving
		// alone a job whose own agency's repository has a script of the name. A
		// job that HAS a script is never moved from it to another: only a job
		// with none is joined.
		//
		// "No script" is a NULL, or a uid that names no row: the composer reads
		// the script before it opens its transaction, so a prune between the two
		// can leave a job holding the uid of a script that has just gone, written
		// after the trigger that would have cleared it had already run.
		const scriptless = `source = 'cronomicon' AND script_ref = ?
				    AND (script_uid IS NULL OR script_uid NOT IN (SELECT uid FROM scripts))`
		var rejoinErr error
		switch {
		case s.repo() != repoid.Global:
			_, rejoinErr = tx.ExecContext(ctx,
				`UPDATE jobs SET script_uid = ? WHERE `+scriptless+` AND `+jobsAgencySQL+` = ?`,
				rs.uid, name, s.agency())
		case otherRepositories(ctx, tx):
			_, rejoinErr = tx.ExecContext(ctx,
				`UPDATE jobs SET script_uid = ? WHERE `+scriptless+`
				    AND NOT EXISTS (SELECT 1 FROM scripts x JOIN git_repos g ON g.id = x.repo_id
				                     WHERE x.name = jobs.script_ref AND g.id <> ?
				                       AND g.agency_id = `+jobsAgencySQL+`)`,
				rs.uid, name, repoid.Global)
		default:
			_, rejoinErr = tx.ExecContext(ctx, `UPDATE jobs SET script_uid = ? WHERE `+scriptless, rs.uid, name)
		}
		if rejoinErr != nil {
			return fmt.Errorf("rejoin in-app jobs to script %q: %w", name, rejoinErr)
		}
		// An in-app job follows its script (the owner's decision of 2026-10-09).
		// A job holds a COPY of its script: the run type, the body or the path to
		// it, the executor, the hash. upsertJobs rewrites that copy for a Git job
		// on every sync; nothing rewrote it for a job built in the app, which went
		// on running the body it had when it was last saved while the catalogue
		// showed the new one. Every in-app job joined to this script
		// (jobs.script_uid, 1290) now takes the copy as it is written here. A
		// binned job too, so that restoring it does not bring an old body back.
		// last_modified_* is left alone: nobody edited the job.
		//
		// The columns are exactly the ones the composer copies when the job is
		// saved (api.writeComposedJob). project_root, the checkout marker, is NOT
		// among them and is not written here: the composer has never copied it,
		// so an in-app job on a project is not a checkout job, and making it one
		// is a change of its own that this does not make.
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs
			    SET run_type = ?, command = ?, script = ?, script_path = ?, executor = ?,
			        content_hash = ?
			  WHERE source = 'cronomicon' AND script_uid = ?`,
			rs.runType, nullStr(rs.command), nullStr(rs.script), nullStr(rs.scriptPath), nullStr(rs.executor),
			rs.contentHash, rs.uid); err != nil {
			return fmt.Errorf("refresh in-app jobs of script %q: %w", name, err)
		}
		// And a job of ANOTHER repository that uses this script (GR-16: an
		// agency's job may use Global's). Its copy was written by its own
		// repository's last sync and would otherwise be the old body until that
		// repository synced again. The columns are the ones upsertJobs copies for
		// a Git job, the checkout marker among them; the executor as upsertJobs
		// stores it (the script's when the script has one).
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs
			    SET run_type = ?, command = ?, script = ?, script_path = ?,
			        executor = COALESCE(?, executor), content_hash = ?, project_root = ?
			  WHERE source = 'git' AND script_uid = ? AND COALESCE(repo_id, ?) <> ?`,
			rs.runType, nullStr(rs.command), nullStr(rs.script), nullStr(rs.scriptPath), nullStr(rs.executor),
			rs.contentHash, nullStr(rs.projectRoot), rs.uid, repoid.Global, s.repo()); err != nil {
			return fmt.Errorf("refresh other repositories' jobs of script %q: %w", name, err)
		}
	}
	return nil
}

// resolvedSchedule is a first-class Schedule with its content hash computed (A10a).
type resolvedSchedule struct {
	// uid is the schedule's identity in the schedules table. Empty until
	// upsertSchedules has written the row, which fills it in (the standing uid
	// of a row that was there, a new one otherwise). mergeScheduleRefs copies it
	// onto every entry expanded from this schedule.
	uid         string
	cron        string
	env         map[string]string
	description string
	contentHash string
	sourcePath  string
	// startAt/endAt are the optional activation window (AW-8), copied onto every
	// entry expanded from this schedule.
	startAt string
	endAt   string
	// interval is the Phase 2 anchored-interval mode, mutually exclusive with cron.
	interval string
	// skipCals/onlyCals are the working-calendar bindings (CAL-9), copied onto
	// every entry expanded from this schedule like cron/env/window.
	skipCals []string
	onlyCals []string
}

// ScheduleContentHash exposes scheduleContentHash to the API write path so an
// operator-authored (source='cronomicon') schedule computes the identical digest a
// git-synced one does — single-sourcing the hash algorithm (D8 — schedule-builder.md).
func ScheduleContentHash(cron string, env map[string]string, startAt, endAt, interval string, skipCals, onlyCals []string) string {
	return scheduleContentHash(cron, env, startAt, endAt, interval, skipCals, onlyCals)
}

// scheduleContentHash is the Decision-8-style hash of a schedule's cron + env +
// activation window, for run-snapshot reproducibility. json.Marshal sorts map
// keys, so it is deterministic.
//
// The window (AW-6) and the interval (Phase 2) are appended ONLY when set, and
// in that order, so a plain cron schedule — every row authored before these
// features existed — hashes byte-identically to what is already stored. That
// keeps the column stable across both upgrades with no recompute pass and no
// spurious "modified" flag on the first sync after deploy. (An interval-mode
// row always carries a window, since the anchor is required, so the window
// segment is never skipped ahead of a present interval segment.)
func scheduleContentHash(cron string, env map[string]string, startAt, endAt, interval string, skipCals, onlyCals []string) string {
	envJSON := ""
	if len(env) > 0 {
		b, _ := json.Marshal(env)
		envJSON = string(b)
	}
	payload := cron + "\x00" + envJSON
	if startAt != "" || endAt != "" {
		payload += "\x00" + startAt + "\x00" + endAt
	}
	if interval != "" {
		payload += "\x00" + interval
	}
	// CAL-5 — the calendar bindings ride the SAME conditional-segment rule, for
	// the same reason: an entry with no bindings must hash byte-identically to
	// what is already stored, or the first sync after deploy reports the entire
	// catalogue as modified. Each polarity is its own segment and each is
	// appended only when non-empty, so binding ONLY a skip calendar does not
	// disturb the position of anything else.
	// Each segment is POLARITY-TAGGED. Untagged, skip:["holidays"] and
	// only:["holidays"] produce the identical payload — two opposite firing rules
	// with one digest, so flipping "never run on holidays" to "only run on
	// holidays" would leave content_hash unchanged and every drift check blind to
	// it. The tag costs nothing and only ever appears on rows that have a binding,
	// so the no-drift guarantee for unbound rows is untouched.
	if skip := calendar.MarshalNames(skipCals); skip != "" {
		payload += "\x00skip:" + skip
	}
	if only := calendar.MarshalNames(onlyCals); only != "" {
		payload += "\x00only:" + only
	}
	return ContentHash([]byte(payload))
}

// resolveSchedules computes each first-class schedule's content hash, returning a
// name→resolved map plus any errors. A schedule missing metadata.name is omitted so
// its referencing owners surface as dangling.
func (s *Service) resolveSchedules(scheds []ScheduleYAML) (map[string]resolvedSchedule, []error) {
	out := make(map[string]resolvedSchedule, len(scheds))
	var errs []error
	for _, sd := range scheds {
		name := sd.Metadata.Name
		if name == "" {
			errs = append(errs, ValidationError{Field: "metadata.name", Message: "schedule is missing metadata.name"})
			continue
		}
		// AW-8: a malformed window is advisory, never a sync-wedging failure —
		// the entry syncs unbounded (the pre-window behavior) and the operator
		// sees the error rather than losing the whole repo's schedules.
		startAt, endAt, werr := NormalizeWindow(sd.Spec.StartAt, sd.Spec.EndAt)
		if werr != nil {
			errs = append(errs, ValidationError{Field: "spec.startAt",
				Message: fmt.Sprintf("schedule %q: %v (window ignored)", name, werr)})
			startAt, endAt = "", ""
		}
		interval := strings.TrimSpace(sd.Spec.Interval)
		// Mode validation is advisory at sync time, like the window above: a bad
		// spec must not wedge a whole repo. The row is written so the operator can
		// see and fix it; the scheduler skips an unparseable entry at reload.
		if serr := (cronutil.Spec{
			Cron: sd.Spec.Cron, Interval: interval,
			Window: cronutil.NewWindow(parseRFC3339Ptr(startAt), parseRFC3339Ptr(endAt)),
		}).Validate(); serr != nil {
			errs = append(errs, ValidationError{Field: "spec.interval",
				Message: fmt.Sprintf("schedule %q: %v", name, serr)})
		}
		out[name] = resolvedSchedule{
			cron:        sd.Spec.Cron,
			env:         sd.Spec.Env,
			description: sd.Spec.Description,
			contentHash: scheduleContentHash(sd.Spec.Cron, sd.Spec.Env, startAt, endAt, interval, sd.Spec.SkipCalendars, sd.Spec.OnlyCalendars),
			sourcePath:  defPath(sd.SourcePath, "schedules/"+name+".yaml"),
			startAt:     startAt,
			endAt:       endAt,
			interval:    interval,
			skipCals:    sd.Spec.SkipCalendars,
			onlyCals:    sd.Spec.OnlyCalendars,
		}
	}
	return out, errs
}

// upsertSchedules writes the resolved first-class schedules into the schedules
// cache table as source='git' rows (operator 'cronomicon' rows are left untouched).
//
// NOTE: schedules.tags (migration 290) is deliberately ABSENT here. Like
// scripts.tags (280) / jobs.tags (D6), tags are user-authored (PUT /schedule-tags),
// SQLite-only, and must survive syncs: the column DEFAULT '[]' covers a new row and
// omission from DO UPDATE preserves an existing row's tags. Do NOT add tags here —
// TestScheduleTagsSurviveSync guards this.
func (s *Service) upsertSchedules(ctx context.Context, tx *sql.Tx, resolved map[string]resolvedSchedule, now string, sha string) error {
	for name, rs := range resolved {
		var envJSON any
		if len(rs.env) > 0 {
			b, err := json.Marshal(rs.env)
			if err != nil {
				return fmt.Errorf("marshal schedule env %q: %w", name, err)
			}
			envJSON = string(b)
		}
		// A Git schedule is its repository's agency's, and unique by name within
		// that owner (migration 1300, GR-6): two repositories may each supply a
		// `nightly`. owner_agency and repo_id are written at first sight and are
		// ABSENT from the update, like the uid: a repository's agency never
		// changes (GR-2).
		_, err := tx.ExecContext(ctx, `
			INSERT INTO schedules(name, source, description, cron, env, content_hash, source_path, synced_at, start_at, end_at, interval, skip_calendars, only_calendars, uid,
			                      owner_agency, repo_id)
			VALUES(?, 'git', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source, owner_agency, name) DO UPDATE SET
				description=excluded.description,
				cron=excluded.cron,
				env=excluded.env,
				content_hash=excluded.content_hash,
				source_path=excluded.source_path,
				synced_at=excluded.synced_at,
				start_at=excluded.start_at,
				end_at=excluded.end_at,
				interval=excluded.interval,
				skip_calendars=excluded.skip_calendars,
				only_calendars=excluded.only_calendars`,
			name, nullStr(rs.description), rs.cron, envJSON, rs.contentHash, nullStr(rs.sourcePath), now,
			nullStr(rs.startAt), nullStr(rs.endAt), nullStr(rs.interval),
			// Without these the catalog row is permanently NULL for bindings while
			// the runtime rows carry them — and resolveComposeSchedules reads the
			// CATALOG, so an in-app job binding a Git-authored schedule would
			// silently lose its calendar policy. That is the §2.5 reusable-policy
			// story failing exactly across the source boundary.
			nullStr(calendar.MarshalNames(rs.skipCals)), nullStr(calendar.MarshalNames(rs.onlyCals)),
			// AF-4a — the surrogate identity, assigned at first sight and OMITTED
			// from DO UPDATE so a re-sync never re-mints it (same preservation-by-
			// omission as tags). NEVER derived from the name: the whole point is
			// an identity that survives what happens to names.
			db.NewID(), s.agency(), s.repo())
		if err != nil {
			return fmt.Errorf("upsert schedule %q: %w", name, err)
		}
		// The blind upsert cannot say whether it inserted or updated, so the uid
		// is read back, for the entries expanded from this schedule below.
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(uid, '') FROM schedules WHERE source = 'git' AND owner_agency = ? AND name = ?`,
			s.agency(), name).Scan(&rs.uid); err != nil {
			return fmt.Errorf("resolve uid for schedule %q: %w", name, err)
		}
		resolved[name] = rs
		// An in-app definition bound to this schedule follows its edits (2.4.0,
		// present defect 13). A definition holds a COPY of its schedule's timing.
		// The Git definitions' copies are rewritten further down this sync, with
		// the definitions themselves; an in-app definition's was written when it
		// was last saved and by nothing since, so it went on firing at the old
		// time after the schedule was edited in Git, where an in-app schedule's
		// edit has always reached its users (D1c). The same columns that edit
		// propagates, found the same way: by schedule_uid.
		//
		// And a definition of ANOTHER repository that uses this schedule (2.4.0,
		// GR-16: an agency's definition may use Global's). Its copy was written
		// by its own repository's last sync. This repository's own definitions
		// are left to the rewrite further down, as before.
		const elsewhereOwner = `(owner_source = 'cronomicon' OR owner_uid IN (
			SELECT uid FROM jobs      WHERE source = 'git' AND repo_id IS NOT NULL AND repo_id <> ?
			UNION ALL
			SELECT uid FROM workflows WHERE source = 'git' AND repo_id IS NOT NULL AND repo_id <> ?))`
		if _, err := tx.ExecContext(ctx, `
			UPDATE definition_schedules
			   SET cron=?, env=?, start_at=?, end_at=?, interval=?, skip_calendars=?, only_calendars=?
			 WHERE schedule_uid = ? AND `+elsewhereOwner,
			rs.cron, envJSON, nullStr(rs.startAt), nullStr(rs.endAt), nullStr(rs.interval),
			nullStr(calendar.MarshalNames(rs.skipCals)), nullStr(calendar.MarshalNames(rs.onlyCals)),
			rs.uid, s.repo(), s.repo()); err != nil {
			return fmt.Errorf("propagate schedule %q to the definitions that use it: %w", name, err)
		}
		// The legacy display mirror (jobs/workflows.schedule, the lowest-position
		// cron) of each owner just touched, as the in-app edit recomputes it.
		for kind, table := range map[string]string{"job": "jobs", "workflow": "workflows"} {
			if _, err := tx.ExecContext(ctx, `
				UPDATE `+table+`
				   SET schedule = (SELECT d.cron FROM definition_schedules d
				                    WHERE d.owner_kind = ? AND d.owner_uid = `+table+`.uid
				                    ORDER BY d.position LIMIT 1)
				 WHERE (source = 'cronomicon' OR (repo_id IS NOT NULL AND repo_id <> ?))
				   AND uid IN (SELECT owner_uid FROM definition_schedules
				                WHERE schedule_uid = ? AND owner_kind = ?)`,
				kind, s.repo(), rs.uid, kind); err != nil {
				return fmt.Errorf("refresh the schedule column of the %ss bound to %q: %w", kind, name, err)
			}
		}
	}
	return nil
}

// mergeScheduleRefs appends entries for each resolved scheduleRef to an owner's
// inline schedule entries (A10a). Names already present (inline) win; a ref whose
// name collides with an inline entry is skipped (the inline entry is canonical).
// Danglers were filtered upstream, so an unresolved ref is silently ignored here.
func mergeScheduleRefs(entries []ScheduleEntry, refs []string, resolved map[string]resolvedSchedule) []ScheduleEntry {
	seen := map[string]bool{}
	for _, e := range entries {
		seen[strings.ToLower(e.Name)] = true
	}
	for _, ref := range refs {
		rs, ok := resolved[ref]
		if !ok || seen[strings.ToLower(ref)] {
			continue
		}
		seen[strings.ToLower(ref)] = true
		// SourceRef records the first-class schedule this entry was expanded from
		// (D1c) so a later schedule edit propagates precisely to this row. The
		// activation window is copied down with cron/env (AW-5) so the runtime
		// row is self-contained — the scheduler never joins back to `schedules`.
		entries = append(entries, ScheduleEntry{
			Name: ref, Cron: rs.cron, Env: rs.env, SourceRef: ref, SourceUID: rs.uid,
			StartAt: rs.startAt, EndAt: rs.endAt, Interval: rs.interval,
			SkipCalendars: rs.skipCals, OnlyCalendars: rs.onlyCals,
		})
	}
	return entries
}

// defPath returns the discovered source path, or a name-derived fallback for a
// definition with no SourcePath (Compose-authored, or a flat file). Recursive
// discovery sets SourcePath to the real nested path so the UI can group by folder.
func defPath(sourcePath, fallback string) string {
	if sourcePath != "" {
		return sourcePath
	}
	return fallback
}

// upsertJobs writes the resolved jobs into the jobs cache table.
//
// NOTE: jobs.tags is deliberately ABSENT from both the column list and the
// ON CONFLICT DO UPDATE SET below (tags-support.md D6). Tags are now operator-owned
// — authored via PUT /api/v1/job-tags/{jobId}, stored only in SQLite, and must
// survive syncs exactly like scripts.tags (280). The column DEFAULT '[]' covers a
// freshly-inserted git job and omission from DO UPDATE preserves an existing row's
// tags. YAML `spec.tags` is parsed-but-unused (kept on the spec struct). The cronomicon
// JobComposer compose upsert is separate and still authors tags. Do NOT add tags
// here — TestJobTagsSurviveSync guards this.
func (s *Service) upsertJobs(ctx context.Context, tx *sql.Tx, jobs []JobYAML, resolved map[string]resolvedScript, resolvedScheds map[string]resolvedSchedule, now string, sha string) error {
	for _, j := range jobs {
		name := j.Metadata.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(j.Spec.TargetHost), ".yaml")
		}
		enabled := 1
		if j.Spec.Enabled != nil && !*j.Spec.Enabled {
			enabled = 0
		}
		concPolicy := j.Spec.ConcurrencyPolicy
		if concPolicy == "" {
			concPolicy = "Allow"
		}
		concKey := j.Spec.ConcurrencyKey
		requestable := 0
		if j.Spec.Requestable {
			requestable = 1
		}
		// Resolve multi-schedule entries; mirror the lowest-position (first) entry's
		// cron into the legacy `schedule` column for display, preserving 'Manual'/
		// empty when there are no entries.
		entries, _ := NormalizeSchedules(j.Spec.Schedule, j.Spec.Schedules)
		entries = mergeScheduleRefs(entries, j.Spec.ScheduleRefs, resolvedScheds)
		legacyMirror := j.Spec.Schedule
		if len(entries) > 0 {
			legacyMirror = entries[0].Cron
		}
		// Resolve the executable fields written to the jobs cache. With a
		// script_ref, denormalize run_type/body/executor + content_hash from the
		// referenced script (Decision 7) so the scheduler, execspec, and API
		// serializers keep reading jobs.* unchanged. Otherwise use the job's own
		// inline body and hash it. Danglers were filtered by the caller, so a
		// script_ref here is guaranteed present in `resolved`.
		runType := j.Spec.RunType
		command, script, scriptPath, executor := j.Spec.Command, j.Spec.Script, j.Spec.ScriptPath, storableExecutor(j.Spec.Executor)
		// Where the stored executor came from, for the warning below.
		executorFrom := "the job's file"
		var scriptRef, scriptUID, contentHash, projectRoot string
		if j.Spec.ScriptRef != "" {
			rs := resolved[j.Spec.ScriptRef]
			// Which script the name means: the row upsertScripts wrote earlier in
			// this transaction (1290). The name stays beside it, as authored.
			scriptUID = rs.uid
			runType = rs.runType
			command, script, scriptPath = rs.command, rs.script, rs.scriptPath
			// The script's executor was the one a run read (Decision 7), so it is
			// the one stored. A job that sets its own over a script that sets none
			// keeps its own: the value was never read there, and is kept only so
			// the leftover_executor_key notice names every job whose FILE still
			// carries the line.
			if rs.executor != "" {
				executor, executorFrom = rs.executor, "its script's sidecar ("+rs.sourcePath+")"
			}
			scriptRef = j.Spec.ScriptRef
			contentHash = rs.contentHash
			// §7 — denormalize the checkout marker from the referenced script (the
			// content_hash precedent) so enqueue + read APIs tell a checkout job
			// from a body job without a scripts join. A project script sets
			// script_path = entry, so scriptPath already carries the entry path.
			projectRoot = rs.projectRoot
		} else if h, herr := s.resolveBodyHash(j.Spec.Command, j.Spec.Script, j.Spec.ScriptPath); herr == nil {
			contentHash = h
		}
		// TG-4 — an ansible job's target_host is passed verbatim to `ansible --limit`,
		// which refuses any name carrying a pattern metacharacter; a refused pin means
		// NO --limit, i.e. the run would widen to the full inventory. Warn, never
		// block: a sync must not wedge a whole repo over one bad field (and the row is
		// still written, so the operator can see and fix it). Runs of such a job are
		// hard-refused at the manifest boundary with a 409, which is what makes this
		// advisory posture safe. Checked against the EFFECTIVE run type resolved above
		// — with a script_ref the script's run type is what gets stored.
		if runType == "ansible" && j.Spec.TargetHost != "" && !inventory.ValidName(j.Spec.TargetHost) {
			s.logWarn("git sync: job target_host cannot be expressed as an ansible --limit; runs will be refused (409) rather than targeting the full inventory",
				"job", j.Metadata.Name, "source_path", j.SourcePath, "target_host", j.Spec.TargetHost)
		}
		// AF-1 (decision AF-Q2) — a job YAML with no `scope` syncs as All (global,
		// visible to every agency) and says so, rather than being refused. The
		// in-app composer REQUIRES the choice (422 on an absent scope), but Git is
		// a different contract: a definition already merged and reviewed must not
		// go out of service over a metadata field, and refusing it here would wedge
		// the whole repo's sync on one file. The warning is the fix path — the row
		// is written either way, so the operator can see it and add the field.
		if strings.TrimSpace(j.Spec.Scope) == "" {
			s.logWarn("git sync: job declares no scope, so it lands in All — visible to every agency; add `spec.scope` to place it with one department",
				"job", j.Metadata.Name, "source_path", j.SourcePath)
		}
		// LR-71 — a fixed target_host must be one of the job's scope's hosts, or
		// its runs fail for that host (execspec.ResolveTargets). A WARNING, never an
		// error: a sync error would mark the sync partial and suppress pruning over
		// one field, and the run-time refusal is the control. The standing record
		// is the notice the inbox raises (target_host_outside_scope); this line is
		// for whoever reads the sync log. Skipped when the scope is not in the
		// catalog yet: there is no membership to ask about until it is.
		if th := strings.TrimSpace(j.Spec.TargetHost); th != "" && strings.TrimSpace(j.Spec.Scope) != "" {
			var member bool
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = sc.id AND sh.host = ?)
				    OR NOT EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = sc.id)
				  FROM scopes sc WHERE sc.name = ?`, th, j.Spec.Scope).Scan(&member); err == nil && !member {
				s.logWarn("git sync: job target_host is not a host of its scope; its runs will fail for that host until the host is added to the scope or the job names one of the scope's hosts",
					"job", j.Metadata.Name, "source_path", j.SourcePath, "scope", j.Spec.Scope, "target_host", th)
			}
		}
		// CA-8 — advisory warnings for the declarative "connect as" identity;
		// like TG-4, a sync must never wedge a repo over one field, and both
		// conditions fail loudly at the run boundary anyway (422 at trigger /
		// enqueue-fold skip for ansible-terraform, missing-label failure at
		// dispatch per CA-3). The row is still written so the operator can see
		// and fix the value in place.
		sshUser, sshCred := j.Spec.SSHUser, j.Spec.SSHCredential
		if (sshUser != "" || sshCred != "") && !execspec.IdentityCapableRunType(runType) {
			s.logWarn("git sync: ssh_user/ssh_credential cannot be applied to this run type (terraform authenticates through its providers, not SSH); the field is stored but ignored for this job's runs",
				"job", j.Metadata.Name, "source_path", j.SourcePath, "run_type", runType)
		}
		// Asked within the repository's agency (2.4.0): what a job of an agency's
		// repository can use is its agency's and Global's, and a warning that
		// said otherwise would tell one agency which names another holds.
		// Global's repository's jobs may name any agency's scope, so for those
		// the question is the old one.
		ownerArm := ""
		ownerArgs := []any{}
		if s.repo() != repoid.Global {
			ownerArm = ` AND owner_agency IN (?, ?)`
			ownerArgs = []any{s.agency(), agencyid.Global}
		}
		if sshCred != "" {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM ssh_credentials WHERE label = ?`+ownerArm+` LIMIT 1`,
				append([]any{sshCred}, ownerArgs...)...).Scan(&one); err != nil {
				s.logWarn("git sync: job ssh_credential names no stored SSH credential; runs will fail at dispatch until it exists",
					"job", j.Metadata.Name, "source_path", j.SourcePath, "ssh_credential", sshCred)
			}
		}
		// RA-12 — same ADVISORY discipline as ssh_credential above: a become password
		// naming a secret that does not exist (yet) is a sync FINDING, never a sync
		// failure. Git is often the first place the reference appears — the catalogue
		// row may be created minutes later by a different person — and failing the
		// whole sync would take every other job in the repo down with it. Dispatch is
		// where it fails closed, loudly, for that one run.
		if bp := strings.TrimSpace(j.Spec.BecomePasswordSecret); bp != "" {
			if runType != "ansible" {
				s.logWarn("git sync: become_password_secret applies only to ansible runs; the field is stored but ignored for this job's runs",
					"job", j.Metadata.Name, "source_path", j.SourcePath, "run_type", runType)
			}
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM secrets WHERE key = ?`+ownerArm+` LIMIT 1`,
				append([]any{bp}, ownerArgs...)...).Scan(&one); err != nil {
				s.logWarn("git sync: job become_password_secret names no stored Secret; runs will fail closed at dispatch until it exists",
					"job", j.Metadata.Name, "source_path", j.SourcePath, "become_password_secret", bp)
			}
		}
		// ET-D — advisory-warn, like the deadline above: a malformed watch must not
		// fail the sync for the whole repo (PP-B2), and a job whose watch was
		// dropped simply is not file-triggered, which the eligibility report shows.
		watches := watchspec.Normalize(j.Spec.Watch)
		if verrs := watchspec.Validate(watches); len(verrs) > 0 {
			for _, ve := range verrs {
				s.logWarn("job declares an unusable file watch; ignoring it", "job", name, "source_path", j.SourcePath, "problem", ve)
			}
			watches = nil
		}
		watchJSON := watchspec.Marshal(watches)
		// LR-48 — `executor` is retired: stored (the 2.3.0 upgrade pass reads
		// what jobs used to say) and read by nothing else. Said at sync, because
		// the decode is not strict and the key would otherwise be ignored in
		// silence; the inbox carries it too (notices: leftover_executor_key),
		// since nobody reads a sync log. A value the key never had is stored as
		// nothing (storableExecutor) and is validation's to report, by file and
		// line.
		if executor != "" {
			s.logWarn("git sync: job still declares executor, which is no longer read — every job is taken by "+
				"whichever runner that serves its scope asks first; remove the line, and bind the scope to the "+
				"runners that should serve it",
				"job", name, "source_path", j.SourcePath, "scope", j.Spec.Scope, "executor", executor,
				"declared_in", executorFrom)
		}
		warnDeadline := strings.TrimSpace(j.Spec.MustFinishBy)
		if warnDeadline != "" && !cronutil.ValidDeadline(warnDeadline) {
			s.logWarn("job declares an unparseable must_finish_by; ignoring it",
				"job", name, "source_path", j.SourcePath, "value", warnDeadline)
			warnDeadline = ""
		}
		// SB — spec.runner_tag is RETIRED. It used to confine the job to runners
		// carrying that tag; where a job runs is now decided by its scope's runner
		// binding (mig. 1180), and this key is read only to say so. It is still a
		// recognised field on purpose: the YAML decode is not strict, so a key the
		// struct forgot would vanish without a word, and the author of a job that
		// says `runner_tag: vlan-dmz` would go on believing it is confined.
		//
		// Never an error (owner decision): the job syncs either way. What differs
		// is how loud the warning is. On a bound scope the line is merely stale —
		// the job runs on the scope's runners. On an unbound one, or with no scope,
		// nothing confines the job any more, and that case also goes on the
		// notice list below so it is seen somewhere other than this log.
		leftoverPin := strings.TrimSpace(j.Spec.RunnerTag)
		leftoverPinUnconfined := false
		if leftoverPin != "" {
			bound, berr := execspec.ScopeIsBound(ctx, tx, j.Spec.Scope)
			switch {
			case berr != nil:
				// Unreadable: say only what is certain. No notice is written on a
				// guess in either direction.
				s.logWarn("git sync: job still declares runner_tag, which is no longer read; remove the line",
					"job", name, "source_path", j.SourcePath, "runner_tag", leftoverPin)
			case bound:
				s.logWarn("git sync: job still declares runner_tag, which is no longer read; its scope is bound to "+
					"runners and it runs on those — remove the line",
					"job", name, "source_path", j.SourcePath, "scope", j.Spec.Scope, "runner_tag", leftoverPin)
			default:
				leftoverPinUnconfined = true
				s.logWarn("git sync: job still declares runner_tag, which is no longer read; NOTHING CONFINES THIS JOB "+
					"NOW — its scope is not bound to any runner, so it may run on any runner in its agency. Bind the "+
					"scope's runners, or remove the line if that is intended",
					"job", name, "source_path", j.SourcePath, "scope", j.Spec.Scope, "runner_tag", leftoverPin)
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs(name, run_type, description, scope, target_host, schedule,
			                 enabled, timeout_seconds, retries, requestable,
			                 concurrency_policy, concurrency_key,
			                 command, script, script_path, executor,
			                 script_ref, content_hash,
			                 source_path, synced_at, prompts_json, env_passthrough,
			                 project_root, requires_json, prompt_enforcement,
			                 ssh_user, ssh_credential, become_password_secret,
			                 warn_after_seconds, must_finish_by, watch_json, uid, script_uid, repo_id)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(repo_id, name) WHERE source = 'git' DO UPDATE SET
				run_type=excluded.run_type,
				description=excluded.description,
				scope=excluded.scope,
				target_host=excluded.target_host,
				schedule=excluded.schedule,
				enabled=excluded.enabled,
				timeout_seconds=excluded.timeout_seconds,
				retries=excluded.retries,
				requestable=excluded.requestable,
				concurrency_policy=excluded.concurrency_policy,
				concurrency_key=excluded.concurrency_key,
				command=excluded.command,
				script=excluded.script,
				script_path=excluded.script_path,
				executor=excluded.executor,
				script_ref=excluded.script_ref,
				script_uid=excluded.script_uid,
				content_hash=excluded.content_hash,
				source_path=excluded.source_path,
				synced_at=excluded.synced_at,
				prompts_json=excluded.prompts_json,
				env_passthrough=excluded.env_passthrough,
				project_root=excluded.project_root,
				requires_json=excluded.requires_json,
				prompt_enforcement=excluded.prompt_enforcement,
				ssh_user=excluded.ssh_user,
				ssh_credential=excluded.ssh_credential,
				become_password_secret=excluded.become_password_secret,
				warn_after_seconds=excluded.warn_after_seconds,
				must_finish_by=excluded.must_finish_by,
				watch_json=excluded.watch_json`,
			name, runType, j.Spec.Description, j.Spec.Scope, j.Spec.TargetHost,
			nullStr(legacyMirror), enabled,
			j.Spec.TimeoutSeconds, j.Spec.Retries, requestable,
			concPolicy, nullStr(concKey),
			nullStr(command), nullStr(script), nullStr(scriptPath),
			nullStr(executor), nullStr(scriptRef), nullStr(contentHash),
			defPath(j.SourcePath, "jobs/"+name+".yaml"), now, MarshalPrompts(j.Spec.Prompts),
			MarshalEnvPassthrough(j.Spec.EnvPassthrough), nullStr(projectRoot),
			MarshalRequires(j.Spec.Requires),
			// JR-Q5 — unrecognized values degrade to "warn" so a typo in Git can never
			// silently take a job offline.
			NormalizePromptEnforcement(j.Spec.PromptEnforcement),
			nullStr(sshUser), nullStr(sshCred), nullStr(strings.TrimSpace(j.Spec.BecomePasswordSecret)),
			// SL — soft deadlines. A malformed must_finish_by is advisory-warned rather
			// than failing the sync, matching the become-password precedent above: a
			// typo in one job's deadline must not stop the whole repo from syncing.
			nullIfZero(j.Spec.WarnAfterSeconds), nullStr(warnDeadline), watchJSON,
			// AF-4a — see the schedules upsert: first-sight identity, preserved on
			// conflict by omission from DO UPDATE.
			db.NewID(), nullStr(scriptUID),
			// The repository the job comes from (GR-3, migration 1310): written
			// at first sight and absent from DO UPDATE, like the uid.
			s.repo())
		if err != nil {
			return fmt.Errorf("upsert job %q: %w", name, err)
		}
		// LU-6: allocate this job's log-folder code. Idempotent, so the blind
		// upsert above not being able to tell an insert from an update doesn't
		// matter — and it self-heals a row that predates the registry. NOTE the
		// post-fallback `name` local is the identity, not j.Metadata.Name: an
		// empty metadata name degrades to the target-host basename above, and the
		// code must key on whatever actually lands in the jobs row.
		// R2-5: the allocator keys on the uid; the row was just upserted, so its
		// uid is authoritative here (a Git name is unique within its repository
		// by constraint, migration 1320, and this is THIS repository's row).
		var jobUID string
		if err := tx.QueryRowContext(ctx,
			`SELECT uid FROM jobs WHERE source='git' AND repo_id = ? AND name = ?`, s.repo(), name).Scan(&jobUID); err != nil {
			return fmt.Errorf("resolve uid for job %q: %w", name, err)
		}
		// SB — the notice list's half of the runner_tag warning above. A job whose
		// leftover pin confines nothing gets one row, so the Scopes view can show
		// it; a job with a row already (from migration 1180, or an earlier sync)
		// is left alone, including one an operator DISMISSED — re-raising that on
		// every sync would make dismissal meaningless. And once the line is gone
		// from the YAML, the notice this sync wrote for it goes too: removing the
		// line is the fix, and nobody should have to dismiss what they fixed.
		// The migration's own rows are never deleted here; they record a pin that
		// existed, whatever the YAML says now.
		//
		// "A row for this job" is one that names this job's uid, or names its NAME
		// and no job that exists: the row of this job as it was before a prune
		// and a return gave it a new uid, or one from before rows carried a uid.
		// A row that names ANOTHER job of this name, which is another
		// repository's, is that job's notice and neither suppresses nor is
		// cleared by this one.
		const thisJobsPin = `job_source = 'git' AND job_name = ?
		                      AND (job_uid = ? OR job_uid IS NULL OR job_uid = ''
		                           OR job_uid NOT IN (SELECT uid FROM jobs))`
		if leftoverPinUnconfined {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO retired_runner_pins (job_uid, job_name, job_source, scope, runner_tag, reason, recorded_at)
				SELECT ?, ?, 'git', ?, ?, 'leftover_git_key', ?
				 WHERE NOT EXISTS (SELECT 1 FROM retired_runner_pins WHERE `+thisJobsPin+`)`,
				jobUID, name, j.Spec.Scope, leftoverPin, now, name, jobUID); err != nil {
				return fmt.Errorf("record leftover runner_tag for job %q: %w", name, err)
			}
		} else if leftoverPin == "" {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM retired_runner_pins
				 WHERE reason = 'leftover_git_key' AND `+thisJobsPin, name, jobUID); err != nil {
				return fmt.Errorf("clear leftover runner_tag notice for job %q: %w", name, err)
			}
		}
		// LU-6 §5.6.1 — a prune/return cycle mints a FRESH uid (first-sight, the
		// tags rule) but must NOT mint a fresh log folder: re-point the surviving
		// live registry row at the new identity before allocating, so Allocate
		// finds it instead of opening a new era.
		if err := s.entityCodeFor(ctx, tx, entitycode.KindJob, name, jobUID); err != nil {
			return fmt.Errorf("entity code for job %q: %w", name, err)
		}
		if err := s.writeDefinitionReactions(ctx, tx, "job", name, jobUID, j.Spec.Reactions); err != nil {
			return fmt.Errorf("write reactions for job %q: %w", name, err)
		}
		if err := s.writeDefinitionSchedules(ctx, tx, "job", name, jobUID, entries); err != nil {
			return fmt.Errorf("write schedules for job %q: %w", name, err)
		}
	}
	return nil
}

// entityCodeFor makes sure a Git definition has its log-folder code, and that
// a definition which was pruned and has come back has the SAME one.
//
// LU-6 §5.6.1 — a prune/return cycle mints a FRESH uid (first-sight, the tags
// rule) but must NOT mint a fresh log folder: the surviving live registry row
// is re-pointed at the new identity before allocating, so Allocate finds it
// instead of opening a new era.
//
// The surviving row is found by its name AMONG THIS REPOSITORY'S codes
// (entity_codes.repo_id, migration 1320). By the name alone, two repositories
// that each have a `nightly` would each take the other's code at every sync,
// and the two jobs would take turns owning one log folder.
func (s *Service) entityCodeFor(ctx context.Context, tx *sql.Tx, kind, name, uid string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity_codes SET uid = ?
		 WHERE kind = ? AND source = 'git' AND name = ? AND deleted_at IS NULL
		   AND COALESCE(repo_id, 'global') = ?`,
		uid, kind, name, s.repo()); err != nil {
		return fmt.Errorf("re-point: %w", err)
	}
	if _, err := entitycode.Allocate(ctx, tx, kind, "git", name, uid); err != nil {
		return fmt.Errorf("allocate: %w", err)
	}
	// Allocate does not know about repositories; the code it has just written
	// (or found, from before the column) is stamped here.
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity_codes SET repo_id = ?
		 WHERE kind = ? AND uid = ? AND deleted_at IS NULL AND repo_id IS NULL`,
		s.repo(), kind, uid); err != nil {
		return fmt.Errorf("stamp the repository: %w", err)
	}
	return nil
}

// pointReactionsAtTheirUpstreams gives every reaction of this repository's
// definitions the uid of the definition it watches, when that definition is
// one of this repository's own. It runs once every job and workflow of the
// sync has been written.
//
// A reaction row is written with its owner, in whatever order the files were
// read, so an upstream that is written LATER in the same sync is not there to
// be found: the reaction was left with no upstream uid until the next sync,
// and the reaction engine then matches it by NAME. With one repository that
// was only late. With two it is wrong: a reaction in one repository that
// watches `nightly` would fire on the other repository's `nightly` as well.
// The repository's own definition of the name comes first (GR-16); a name it
// does not have is left as the writer resolved it.
func (s *Service) pointReactionsAtTheirUpstreams(ctx context.Context, tx *sql.Tx) error {
	repo := s.repo()
	for _, kind := range []string{"job", "workflow"} {
		table := ownerTable(kind)
		if _, err := tx.ExecContext(ctx, `
			UPDATE reactions
			   SET on_uid = (SELECT u.uid FROM `+table+` u
			                  WHERE u.source = 'git' AND u.repo_id = ?1 AND u.name = reactions.on_name)
			 WHERE on_kind = ?2 AND on_source = 'git' AND owner_source = 'git'
			   AND (owner_uid IN (SELECT uid FROM jobs      WHERE source = 'git' AND repo_id = ?1)
			     OR owner_uid IN (SELECT uid FROM workflows WHERE source = 'git' AND repo_id = ?1))
			   AND EXISTS (SELECT 1 FROM `+table+` u
			                WHERE u.source = 'git' AND u.repo_id = ?1 AND u.name = reactions.on_name)`,
			repo, kind); err != nil {
			return err
		}
	}
	return nil
}

// ownerTable is the table a definition of a kind lives in.
func ownerTable(kind string) string {
	if kind == "workflow" {
		return "workflows"
	}
	return "jobs"
}

// clearOwnedRows deletes one Git definition's rows from a satellite table
// (definition_schedules, reactions) before they are written afresh.
//
// By the owner's uid. Two repositories may each have a definition of one name,
// and by the name each sync deleted the other's rows. A row with NO owner uid
// predates the uid, when the one repository there was is the one that is
// Global's now: only Global's sync clears those, by name, as it always did.
func (s *Service) clearOwnedRows(ctx context.Context, tx *sql.Tx, table, ownerKind, ownerName, ownerUID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE owner_kind = ? AND owner_uid = ?`, ownerKind, ownerUID); err != nil {
		return err
	}
	if s.repo() != repoid.Global {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE owner_source = 'git' AND owner_kind = ? AND owner_name = ? AND owner_uid IS NULL`,
		ownerKind, ownerName)
	return err
}

// writeDefinitionSchedules replaces all schedule rows for one definition
// (delete-by-owner + insert) inside the caller's transaction.
// writeDefinitionReactions replaces all reaction rows for one definition
// (delete-by-owner + insert) inside the caller's transaction — the exact shape
// of writeDefinitionSchedules, for the exact same reason.
//
// # Why there is no content hash involved
//
// RX-13's plan text says to "include reactions in the schedule content hash so
// a changed reaction re-syncs". That instruction does not map onto this
// codebase and is deliberately NOT implemented:
//
//   - definition_schedules has no content_hash column at all; runtime entries
//     are rewritten unconditionally every sync, exactly like this function does.
//   - scheduleContentHash covers a FIRST-CLASS schedule, which has no owner and
//     therefore cannot own a reaction.
//   - jobs.content_hash is the SCRIPT BODY hash. Folding a reaction into it
//     would break the invariant that a job's hash equals its script's, which two
//     sync tests pin.
//
// The goal the instruction was reaching for — "a changed reaction re-syncs" —
// is already true by construction here: this is an unconditional
// delete-then-insert per definition per sync, so a reaction edited in the repo
// lands on the next sync with no hash to invalidate.
//
// Source-scoped replace (A9): only this owner's git rows are cleared, so a sync
// can never wipe an operator's in-app reactions on the same definition name.
//
// The owner is named by its uid, which the caller has just read (2.4.0, GR-13):
// see clearOwnedRows. The UPSTREAM a reaction watches is still found by name.
// A Git one: this repository's definition of the name, else Global's (GR-16:
// a name resolves in the definition's own repository, then in Global's), and
// NEVER another repository's. As first built this fell back to "the one Git
// definition of that name, wherever it is", which is another agency's job the
// moment this repository's own does not sync. One built in the app: only when
// one such definition holds the name, as before (its rule is Phase R4's).
func (s *Service) writeDefinitionReactions(ctx context.Context, tx *sql.Tx, ownerKind, ownerName, ownerUID string, entries []ReactionEntry) error {
	const ownerSource = "git"
	if err := s.clearOwnedRows(ctx, tx, "reactions", ownerKind, ownerName, ownerUID); err != nil {
		return err
	}
	// Normalised again here rather than trusting the caller: this function is the
	// only writer, and a defaulted onSource that reached the column raw would
	// violate its CHECK.
	norm, _ := NormalizeReactions(entries)
	for i, e := range norm {
		enabled := 0
		if e.EnabledOrDefault() {
			enabled = 1
		}
		children := 0
		if e.IncludeWorkflowChildren {
			children = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reactions(owner_source, owner_kind, owner_name, name,
			                      on_source, on_kind, on_name, on_outcome,
			                      delay_seconds, min_interval_seconds,
			                      include_workflow_children, enabled, position,
			                      owner_uid, on_uid)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,
				?,
				COALESCE(
					(SELECT uid FROM `+ownerTable(e.OnKind)+` WHERE ? = 'git' AND source = 'git' AND repo_id = ? AND name = ?),
					(SELECT uid FROM `+ownerTable(e.OnKind)+` WHERE ? = 'git' AND source = 'git' AND repo_id = 'global' AND name = ?),
					(SELECT CASE WHEN COUNT(*) = 1 THEN MAX(uid) END FROM `+ownerTable(e.OnKind)+` WHERE ? <> 'git' AND name = ? AND source = ?)))`,
			ownerSource, ownerKind, ownerName, e.Name,
			e.OnSourceOrDefault(), e.OnKind, e.OnName, e.OnOutcome,
			e.DelaySeconds, e.MinIntervalSeconds, children, enabled, i,
			ownerUID,
			e.OnSourceOrDefault(), s.repo(), e.OnName,
			e.OnSourceOrDefault(), e.OnName,
			e.OnSourceOrDefault(), e.OnName, e.OnSourceOrDefault()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) writeDefinitionSchedules(ctx context.Context, tx *sql.Tx, ownerKind, ownerName, ownerUID string, entries []ScheduleEntry) error {
	const ownerSource = "git"
	// Source-scoped replace (A9): only this owner's (source-qualified) rows are
	// cleared, so a git sync can never wipe an operator's cronomicon bindings.
	// And only THIS owner's, by its uid (clearOwnedRows): not those of another
	// repository's definition of the same name.
	if err := s.clearOwnedRows(ctx, tx, "definition_schedules", ownerKind, ownerName, ownerUID); err != nil {
		return err
	}
	for i, e := range entries {
		var envJSON any
		if len(e.Env) > 0 {
			b, err := json.Marshal(e.Env)
			if err != nil {
				return err
			}
			envJSON = string(b)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, env, position, source_ref, start_at, end_at, interval, skip_calendars, only_calendars,
				owner_uid, schedule_uid)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,
				-- The owner, by the uid the caller has just read. It used to be looked
				-- up here by NAME and kept only when one definition held that name.
				?,
				-- The schedule this entry was expanded from, by its uid (1300, GR-8).
				-- It used to be looked up here by NAME and kept only when one
				-- schedule held that name, so a name shared with an in-app schedule
				-- left every Git entry without one. The caller knows which schedule
				-- it expanded: the one this sync wrote.
				?)`,
			ownerSource, ownerKind, ownerName, e.Name, e.Cron, envJSON, i, nullStr(e.SourceRef),
			nullStr(e.StartAt), nullStr(e.EndAt), nullStr(e.Interval),
			nullStr(calendar.MarshalNames(e.SkipCalendars)), nullStr(calendar.MarshalNames(e.OnlyCalendars)),
			ownerUID,
			nullStr(e.SourceUID)); err != nil {
			return err
		}
	}
	return nil
}

// upsertWorkflows writes the resolved workflows into the workflows cache table.
//
// NOTE: workflows.tags (migration 290) is deliberately ABSENT here. Like
// scripts.tags (280) / jobs.tags (D6), tags are user-authored (PUT /workflow-tags),
// SQLite-only, and must survive syncs: the column DEFAULT '[]' covers a new row and
// omission from DO UPDATE preserves an existing row's tags. Do NOT add tags here —
// TestWorkflowTagsSurviveSync guards this.
func (s *Service) upsertWorkflows(ctx context.Context, tx *sql.Tx, wfs []WorkflowYAML, resolvedScheds map[string]resolvedSchedule, now string, sha string) error {
	for _, wf := range wfs {
		name := wf.Metadata.Name
		// R2F-2/R2F-Q3 — names are the law in git, so a hand-written `jobUid` is
		// STRIPPED here, not merely warned about. The steps graph is passed through
		// verbatim into the stored JSON, so leaving it would make the engine honour
		// an identity the validator's warning calls ignored — and pin a git
		// workflow to a uid that means nothing in the next installation the repo is
		// replayed into. ValidateRepo raises the warning; this makes it true.
		stripStepJobUIDs(wf.Spec.Steps)
		stepsJSON, _ := json.Marshal(wf.Spec.Steps)
		// SW/PF-Q23 — the git plane WARNS, it does not reject.
		//
		// Git-authored workflow steps have never been structurally validated:
		// they are parsed as []interface{} and passed through verbatim, so the
		// whole WB-S5 vocabulary check applies only to in-app graphs. Sub-workflow
		// steps make that gap sharper (a bad ref is a step that cannot run), so
		// the shape is checked here — but a failure is a LOG LINE, not a sync
		// error, following the become-password advisory-warn precedent and PP-B2's
		// rule that a broken ref must not be able to wedge a sync for everything
		// else in the repo. The runtime depth ceiling is the real backstop.
		if parsed, perr := workflow.ParseSteps(string(stepsJSON)); perr == nil {
			for _, verr := range workflow.ValidateSteps(parsed) {
				s.logWarn("git workflow has a structural problem; syncing it anyway",
					"workflow", name, "source_path", wf.SourcePath, "step", verr.Step, "field", verr.Field, "problem", verr.Message)
			}
		}
		enabled := 1
		if wf.Spec.Enabled != nil && !*wf.Spec.Enabled {
			enabled = 0
		}
		entries, _ := NormalizeSchedules(wf.Spec.Schedule, wf.Spec.Schedules)
		entries = mergeScheduleRefs(entries, wf.Spec.ScheduleRefs, resolvedScheds)
		legacyMirror := wf.Spec.Schedule
		if len(entries) > 0 {
			legacyMirror = entries[0].Cron
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO workflows(name, description, steps, schedule, enabled, source_path, synced_at, uid, repo_id, owner_agency)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(repo_id, name) WHERE source = 'git' DO UPDATE SET
				description=excluded.description,
				steps=excluded.steps,
				schedule=excluded.schedule,
				enabled=excluded.enabled,
				source_path=excluded.source_path,
				synced_at=excluded.synced_at`,
			name, wf.Spec.Description, string(stepsJSON),
			nullStr(legacyMirror), enabled,
			defPath(wf.SourcePath, "workflows/"+name+".yaml"), now,
			db.NewID(),
			// The repository it comes from (GR-3): see upsertJobs. And its owner,
			// that repository's agency (GR-6, 1360): written at first sight and
			// absent from the update, like the uid, since a repository's agency
			// never changes (GR-2).
			s.repo(), s.agency())
		if err != nil {
			return fmt.Errorf("upsert workflow %q: %w", name, err)
		}
		// LU-6: see upsertJobs. Same idempotent allocation, workflow kind.
		var wfUID string
		if err := tx.QueryRowContext(ctx,
			`SELECT uid FROM workflows WHERE source='git' AND repo_id = ? AND name = ?`, s.repo(), name).Scan(&wfUID); err != nil {
			return fmt.Errorf("resolve uid for workflow %q: %w", name, err)
		}
		if err := s.entityCodeFor(ctx, tx, entitycode.KindWorkflow, name, wfUID); err != nil {
			return fmt.Errorf("entity code for workflow %q: %w", name, err)
		}
		if err := s.writeDefinitionReactions(ctx, tx, "workflow", name, wfUID, wf.Spec.Reactions); err != nil {
			return fmt.Errorf("write reactions for workflow %q: %w", name, err)
		}
		if err := s.writeDefinitionSchedules(ctx, tx, "workflow", name, wfUID, entries); err != nil {
			return fmt.Errorf("write schedules for workflow %q: %w", name, err)
		}
	}
	return nil
}

// It returns the inventories it did NOT write because their name is already a
// scope's: each as that file's error.
func (s *Service) upsertScopes(ctx context.Context, tx *sql.Tx, scopes []inventoryScope, now string, sha string) ([]ValidationError, error) {
	// Scopes table is owned by B6 (settings) but B3 writes git-source rows.
	// Check if the table exists before writing to avoid failing when B6 hasn't
	// been applied yet (test isolation). In production all migrations run before
	// any handler is called.
	var exists int
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='scopes'`).Scan(&exists)
	if exists == 0 {
		return nil, nil // scopes table not yet created — no-op
	}

	// One wording, whoever holds the name and however (GR-19): a scope built in
	// the app, another repository's, another agency's. A repository's
	// committers are told that the name is taken and not whose it is, which is
	// the rule the composers follow (execspec.NamePoolRefusal).
	var taken []ValidationError
	nameTaken := func(sc inventoryScope) {
		taken = append(taken, ValidationError{
			File: sc.SourcePath,
			Message: fmt.Sprintf("the scope name %q is already in use: this inventory was not synced; rename the file",
				sc.Name),
		})
	}
	for _, sc := range scopes {
		// OD-1: a git inventory must NEVER clobber an operator-authored (cronomicon)
		// scope of the same name. The ON CONFLICT(name) upsert below would otherwise
		// flip its source to 'git' and overwrite its authored raw/projection/hosts —
		// a silent hijack. Skip the colliding git scope loudly; the operator renames
		// one. (Names are unique, so an cronomicon row owning the name blocks the git one.)
		var existingSource string
		var existingRepo sql.NullString
		_ = tx.QueryRowContext(ctx, `SELECT source, repo_id FROM scopes WHERE name=?`, sc.Name).Scan(&existingSource, &existingRepo)
		if existingSource == "cronomicon" {
			nameTaken(sc)
			continue
		}
		// Nor a scope that ANOTHER repository supplies (GR-13). A scope's name is
		// unique across the installation, so the upsert below would take the
		// other repository's scope over: its hosts and inventory replaced by this
		// file's, under the agency and the runner bindings the other one has. It
		// is skipped, and says only that the name is in use: whose it is, is not
		// this repository's to learn (GR-19). A Git scope that records no
		// repository is from before a scope recorded one, and is Global's.
		if existingSource == "git" {
			holder := repoid.Global
			if existingRepo.Valid && existingRepo.String != "" {
				holder = existingRepo.String
			}
			if holder != s.repo() {
				nameTaken(sc)
				continue
			}
		}
		// git_meta_json — {owner, sidecarPath, errors}. settings.applyGitMeta is
		// the one reader. (capability_json until migration 1170, when it also
		// carried the run types and their origin.)
		metaJSON, _ := json.Marshal(map[string]any{
			"owner":       sc.Meta.Owner,
			"sidecarPath": sc.Meta.SidecarPath,
			"errors":      sc.Meta.Errors,
		})
		id := db.NewID()
		// agency_id is OPERATOR-OWNED and intentionally absent from both the INSERT
		// column list and the ON CONFLICT DO UPDATE SET below (agency-support.md M1,
		// §3.2 / D-OWN) — so an operator's agency binding survives re-sync, the same
		// operator-owned model as tags. Do NOT add agency_id here.
		//
		// Since 2.4.0 (GR-18) a scope from an AGENCY's repository is that agency's.
		// That is still not written here: the database gives a new scope its
		// repository's agency when the row is inserted (migration 1350), from the
		// repo_id this statement supplies, and refuses to let it be moved. A row
		// this statement UPDATES fires neither.
		//
		// The SAME RULE now covers the scope_agencies join table (migration 670,
		// the agencies plan AG-Q6/T2.9), which supersedes agency_id in
		// Phase 3: **git sync must neither INSERT nor DELETE scope_agencies rows.**
		// A sync that "reconciled" them would silently drop operator-assigned
		// membership on every pull — and because this upsert is ON CONFLICT on
		// scopes(name), it runs on every scope on every sync, so the damage would be
		// total and immediate. There is deliberately no membership reconciliation
		// anywhere in this file; TestSyncPreservesScopeAgencies pins that.
		//
		// And it covers scopes.tags (migration 1160, ST band): operator-owned labels
		// written only by PUT /scope-tags/{scopeId}, never parsed from Git. Do NOT add
		// `tags` to the INSERT column list or the ON CONFLICT DO UPDATE SET below —
		// the column default covers a new row, and an existing row must keep what the
		// operator set. TestSyncPreservesScopeTags pins that.
		//
		// And it covers scope_runners (migration 1180, SB band): the runners a scope
		// is restricted to, written only by PUT /scopes/{scopeId}/runners. Those rows
		// hang off the scope's ID, so what protects them is this statement staying an
		// UPSERT that keeps the row. Do NOT turn it into a delete-and-insert or an
		// INSERT OR REPLACE — a new id cascades the binding away and the scope reopens
		// to its whole agency. TestSyncPreservesScopeRunners pins that.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO scopes(id, name, source, source_path, git_meta_json, synced_at, description, created_by, created_at, repo_id)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET
				source=excluded.source,
				-- The repository travels with the source (GR-3, 1310). The row
				-- this updates is one of this repository's own (the two checks
				-- above skip a name held by a scope built in the app or by
				-- another repository), so this only ever fills a row that has
				-- none yet.
				repo_id=excluded.repo_id,
				source_path=excluded.source_path,
				git_meta_json=excluded.git_meta_json,
				synced_at=excluded.synced_at,
				description=excluded.description`,
			id, sc.Name, "git", sc.SourcePath, string(metaJSON), now, sc.Description, "gitlab", now, s.repo())
		if err != nil {
			// Gracefully skip if columns don't exist yet (schema mismatch in parallel dev).
			s.logWarn("upsert scope skipped", "name", sc.Name, "error", err)
			continue
		}

		// EX.5 — persist inventory host membership into scope_hosts so the
		// in-app SSH executor (which has no clone) can resolve a git scope's
		// targets. Replace the set each sync.
		var scopeID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM scopes WHERE name = ?`, sc.Name).Scan(&scopeID); err != nil {
			s.logWarn("scope id lookup skipped", "name", sc.Name, "error", err)
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM scope_hosts WHERE scope_id = ?`, scopeID); err != nil {
			s.logWarn("scope_hosts clear skipped", "name", sc.Name, "error", err)
			continue
		}
		for _, host := range sc.Hosts {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO scope_hosts(scope_id, host) VALUES(?, ?)`, scopeID, host); err != nil {
				s.logWarn("scope_hosts insert skipped", "name", sc.Name, "host", host, "error", err)
			}
		}

		// M1: persist the byte-exact raw inventory + format so an cronomicon-mode
		// ansible run can ship it to the runner for `-i`. Separate write (not in
		// the scopes upsert above) so a pre-350 schema in isolated tests just logs
		// and continues instead of dropping the whole scope. raw_inventory is
		// secret-free — secret-bearing inventory was rejected at ingest (§4.3).
		if _, err := tx.ExecContext(ctx,
			`UPDATE scopes SET raw_inventory=?, inventory_format=? WHERE id=?`,
			sc.Content, sc.Format, scopeID); err != nil {
			s.logWarn("scope raw_inventory write skipped", "name", sc.Name, "error", err)
		}

		// M2: advisory projection — record projection_status (+ degrade reason/line
		// in projection_json) and replace-the-set the projection tables. Defensive
		// log-and-continue so a pre-360/370 schema in isolated tests doesn't drop
		// the scope. The tree is written only when the projection is usable; on a
		// degrade the raw inventory still ships unchanged, but no half-parsed tree
		// is persisted (§4.2).
		s.writeScopeProjection(ctx, tx, scopeID, sc)

		// M4: import the parsed connection vars into ssh_hosts (source='git') so the
		// in-app SSH executor can dial inventory hosts with zero operator action.
		s.importGitHosts(ctx, tx, scopeID, sc, now)
	}
	return taken, nil
}

// sidecarAuthKeyEnvVar returns the per-scope default auth-key env-var NAME from an
// inventory sidecar (M4 / §9.2), or "" when absent.
func sidecarAuthKeyEnvVar(sc *sidecarYAML) string {
	if sc == nil {
		return ""
	}
	return sc.Spec.AuthKeyEnvVar
}

// authKeyBindings summarizes which imported hosts bind an auth-key env-var NAME
// (for the import-visibility log). NAMES only — never a secret value.
func authKeyBindings(conns []inventory.HostConn) []string {
	var out []string
	for _, hc := range conns {
		if hc.AuthKeyEnvVar != "" {
			out = append(out, hc.Host+"→"+hc.AuthKeyEnvVar)
		}
	}
	return out
}

// importGitHosts upserts ssh_hosts rows (source='git') from a scope's parsed
// connection vars (M4 / §9). It is gated on a usable projection — a degraded
// inventory can't be reliably imported. The upsert leaves status/
// last_checked_at UNTOUCHED (§9.4; a host's approved key is the local runner's
// and is keyed by address, not kept on the row), and
// stamps synced_at=now so the prune-by-owner can reap rows dropped from inventory.
// Best-effort: log-and-continue if the 400 schema isn't present (isolated tests).
func (s *Service) importGitHosts(ctx context.Context, tx *sql.Tx, scopeID string, sc inventoryScope, now string) {
	if sc.Projection.PreviewUnavailable {
		// The inventory DEGRADED — we can't re-parse connection vars, but the hosts
		// are still in the file. A projection degrade is NOT a scopeErr, so the
		// prune-by-owner still runs; re-stamp synced_at on this scope's EXISTING git
		// rows so the prune leaves them, preserving their last-good connection detail
		// (OD-13/§9.4 — it must survive a transient degrade, e.g. a host-range line
		// or one bad [group:vars]).
		if _, err := tx.ExecContext(ctx,
			`UPDATE ssh_hosts SET synced_at=? WHERE scope_id=? AND source='git'`, now, scopeID); err != nil {
			s.logWarn("ssh_hosts degrade re-stamp skipped", "name", sc.Name, "error", err)
		}
		return
	}
	conns := inventory.HostConns(sc.Projection, sc.AuthKeyEnvVar)
	if bound := authKeyBindings(conns); len(bound) > 0 {
		// Visibility: a git inventory binds dial addresses + auth-key NAMEs with no
		// operator action (§9.2 trust boundary). Surface what's bound so operators
		// can review (the imported rows are also visible read-only in SSH Targets).
		s.logWarn("imported inventory hosts bound auth-key NAMEs (review in SSH Targets)", "scope", sc.Name, "bindings", bound)
	}
	for _, hc := range conns {
		port := hc.Port
		if port == 0 {
			port = 22
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var,
			                      source, scope_id, synced_at, created_at, created_by,
			                      last_modified_by, last_modified_at)
			VALUES(?, ?, ?, ?, ?, ?, 'git', ?, ?, ?, 'gitlab', 'gitlab', ?)
			ON CONFLICT(scope_id, hostname) WHERE source='git' DO UPDATE SET
				address          = excluded.address,
				port             = excluded.port,
				username         = excluded.username,
				auth_key_env_var = excluded.auth_key_env_var,
				synced_at        = excluded.synced_at,
				last_modified_at = excluded.last_modified_at`,
			db.NewID(), hc.Host, nullStr(hc.Address), port, nullStr(hc.User), nullStr(hc.AuthKeyEnvVar),
			scopeID, now, now, now); err != nil {
			s.logWarn("ssh_hosts import skipped", "name", sc.Name, "host", hc.Host, "error", err)
		}
	}
}

// writeScopeProjection persists the advisory group/host-var projection + status
// for one scope (M2). Best-effort: every statement logs-and-continues so a
// partial/old schema can't fail the whole sync.
func (s *Service) writeScopeProjection(ctx context.Context, tx *sql.Tx, scopeID string, sc inventoryScope) {
	pr := sc.Projection
	status := "ok"
	var projJSON sql.NullString
	if pr.PreviewUnavailable {
		status = "unavailable"
		if b, err := json.Marshal(map[string]any{"reason": pr.PreviewReason, "line": pr.PreviewLine}); err == nil {
			projJSON = sql.NullString{String: string(b), Valid: true}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE scopes SET projection_status=?, projection_json=? WHERE id=?`, status, projJSON, scopeID); err != nil {
		s.logWarn("scope projection_status write skipped", "name", sc.Name, "error", err)
		return
	}
	// Replace the projection tables via the shared writer (the SAME logic the in-app
	// upload handler uses, M5). Best-effort: a partial/old schema in isolated tests
	// shouldn't fail the whole sync.
	if err := inventory.WriteProjectionTables(ctx, tx, scopeID, pr); err != nil {
		s.logWarn("scope projection write skipped", "name", sc.Name, "error", err)
	}
}

// recordSyncEvent writes a row to git_sync_events and inserts a gitsync activity entry.
//
// Not for a sync that was cut short because its own Service was stopped: the
// connection was rewritten, the repository disconnected or the server is
// shutting down, and "failed: context canceled" in the history, once for every
// save that happened to land during a sync, would describe nothing that went
// wrong. The Service that replaces this one syncs at once and records that.
func (s *Service) recordSyncEvent(ctx context.Context, triggeredBy string, res SyncResult) error {
	if s.db == nil {
		return nil
	}
	if res.Status != "success" && s.lifetime().Err() != nil {
		s.logInfo("git sync: cut short because the repository's sync service was stopped; not recorded", "repo_id", s.repo())
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO git_sync_events(repo_id, triggered_by, sha, status, jobs_synced, scripts_synced, schedules_synced, wfs_synced, scopes_synced, error_message, started_at, finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.repo(),
		triggeredBy,
		nullStr(res.SHA),
		res.Status,
		res.JobsSynced,
		res.ScriptsSynced,
		res.SchedulesSynced,
		res.WfsSynced,
		res.ScopesSynced,
		nullStr(res.ErrorMessage),
		res.StartedAt.Format(time.RFC3339),
		res.FinishedAt.Format(time.RFC3339))
	if err != nil {
		return err
	}

	outcome := "success"
	if res.Status == "failed" {
		outcome = "failure"
	} else if res.Status == "partial" {
		outcome = "warning"
	}

	// The repository and the branch that were synced. Until 1310 both came from
	// two columns of the connection that nothing had written since migration
	// 060, so every installation's line said `infra/job-defs`, `main`.
	repoPath := repoDisplayPath(s.repoURL)
	branch := s.writeBranch(ctx)

	summary := fmt.Sprintf("Synced %d jobs, %d scripts, %d schedules, %d workflows, %d scopes", res.JobsSynced, res.ScriptsSynced, res.SchedulesSynced, res.WfsSynced, res.ScopesSynced)
	if res.Status == "failed" {
		summary = res.ErrorMessage
	} else if res.Status == "partial" {
		summary = fmt.Sprintf("Synced with warnings: %s", res.ErrorMessage)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		At:         now,
		Kind:       "gitsync",
		Outcome:    outcome,
		Actor:      triggeredBy,
		Category:   "Git",
		Action:     "sync",
		Summary:    summary,
		CommitSha:  res.SHA,
		Repository: repoPath,
		Branch:     branch,
	})

	return nil
}

// repoDisplayPath is how a repository is named in the activity stream: the
// path of its URL (`group/project`), without the host, a trailing `.git`, or
// anything else a URL can carry (a user, a password, a query). A URL that is
// not one (a bare path, as tests use) is named by its last element.
func repoDisplayPath(repoURL string) string {
	if repoURL == "" {
		return ""
	}
	p := repoURL
	if u, err := url.Parse(repoURL); err == nil && u.Host != "" {
		p = u.Path
	} else {
		p = filepath.Base(filepath.Clean(repoURL))
	}
	return strings.TrimSuffix(strings.Trim(p, "/"), ".git")
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SyncState is the last-known git sync state from the DB.
type SyncState struct {
	LastSHA      string
	LastSyncedAt string
	LastStatus   string
}

// GetSyncState reads what the last sync of this repository saw.
func (s *Service) GetSyncState(ctx context.Context) (SyncState, error) {
	var st SyncState
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_sha,''), COALESCE(last_synced_at,''), COALESCE(last_status,'') FROM git_repos WHERE id=?`, s.repo()).
		Scan(&st.LastSHA, &st.LastSyncedAt, &st.LastStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	return st, err
}

// ListSyncEvents returns paginated git sync history.
// ListSyncEvents pages the sync-event audit. orderBy is a handler-validated
// ORDER BY clause (sortparam allowlist — TS-20); empty falls back to id DESC.
func (s *Service) ListSyncEvents(ctx context.Context, page, pageSize int, orderBy string) ([]map[string]any, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	if orderBy == "" {
		orderBy = " ORDER BY id DESC"
	}
	offset := (page - 1) * pageSize

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM git_sync_events`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, triggered_by, sha, status, jobs_synced, scripts_synced, schedules_synced, wfs_synced, scopes_synced, error_message, started_at, finished_at
		FROM git_sync_events`+orderBy+` LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var events []map[string]any
	for rows.Next() {
		var (
			id              int
			triggeredBy     string
			sha             sql.NullString
			status          string
			jobsSynced      int
			scriptsSynced   int
			schedulesSynced int
			wfsSynced       int
			scopesSynced    int
			errMsg          sql.NullString
			startedAt       string
			finishedAt      string
		)
		if err := rows.Scan(&id, &triggeredBy, &sha, &status, &jobsSynced, &scriptsSynced, &schedulesSynced, &wfsSynced, &scopesSynced, &errMsg, &startedAt, &finishedAt); err != nil {
			return nil, 0, err
		}
		// The DB row is the internal shape; the wire contract is the spec's
		// GitSyncEvent (timestamp/action/commit/author/message/changes/details,
		// status ∈ success|warning|failure) — translate at this boundary.
		// `files` is omitted: per-file change tracking doesn't exist.
		ev := map[string]any{
			"id":        id,
			"timestamp": startedAt,
			"action":    "Pulled", // syncs pull; pushes are audited in schedule_pushes
			"status":    specSyncStatus(status),
			"commit":    nil,
			"author":    triggeredBy,
			"message":   syncSummary(status, jobsSynced, scriptsSynced, schedulesSynced, wfsSynced, scopesSynced),
			"changes":   syncChanges(status, jobsSynced, scriptsSynced, schedulesSynced, wfsSynced, scopesSynced),
			"details":   errMsg.String,
		}
		if sha.Valid && sha.String != "" {
			ev["commit"] = sha.String
		}
		events = append(events, ev)
	}
	return events, total, rows.Err()
}

// specSyncStatus maps internal sync statuses ("failed"/"partial") to the
// GitSyncEvent enum (success|warning|failure).
func specSyncStatus(s string) string {
	switch s {
	case "failed":
		return "failure"
	case "partial":
		return "warning"
	default:
		return s
	}
}

// syncSummary is the human-readable outcome line for a sync event.
func syncSummary(status string, jobs, scripts, schedules, wfs, scopes int) string {
	switch status {
	case "failed":
		return "Sync failed"
	case "partial":
		return fmt.Sprintf("Partial sync — %d jobs, %d scripts, %d schedules, %d workflows, %d scopes", jobs, scripts, schedules, wfs, scopes)
	default:
		return fmt.Sprintf("Synced %d jobs, %d scripts, %d schedules, %d workflows, %d scopes", jobs, scripts, schedules, wfs, scopes)
	}
}

// syncChanges is the compact counts line; empty for failed syncs (nothing landed).
func syncChanges(status string, jobs, scripts, schedules, wfs, scopes int) string {
	if status == "failed" {
		return ""
	}
	return fmt.Sprintf("jobs: %d · scripts: %d · schedules: %d · workflows: %d · scopes: %d", jobs, scripts, schedules, wfs, scopes)
}

// nullIfZero maps a zero int to SQL NULL, so an omitted optional integer field
// stores as "unset" rather than as a meaningful 0. (SL: warn_after_seconds = 0
// would otherwise read as "warn immediately".)
func nullIfZero(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
