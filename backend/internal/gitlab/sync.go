package gitlab

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/inventory"
	"github.com/ResetSmith/cronomicon/internal/secrets"
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
	db            *sql.DB
	log           *slog.Logger
	cloneDir      string // path to the local git clone
	repoURL       string // full GitLab HTTPS URL
	token         string // AMADEUS_GITLAB_TOKEN (may be empty — unauthenticated)
	webhookSecret string
	Cfg           *config.Config // added for KEK-based webhook secret decryption

	mu      sync.Mutex // guards in-progress sync to prevent overlapping runs
	syncing bool

	// onSyncComplete, when set, is invoked at the end of a successful sync with
	// the synced HEAD SHA so the scheduler can reload newly-synced schedules
	// immediately (it self-gates on SHA change). Optional — nil is a no-op.
	onSyncComplete func(ctx context.Context, sha string)
}

// SetOnSyncComplete installs the post-sync hook (wired in main.go to the
// scheduler's SHA-gated reload). Passing a func value avoids an api→scheduler
// import edge.
func (s *Service) SetOnSyncComplete(f func(ctx context.Context, sha string)) {
	s.onSyncComplete = f
}

// NewService builds the GitLab Service.
//
// cloneDir should be a persistent path (e.g. /var/lib/amadeus/git-cache/job-definitions).
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

// ValidateWebhookToken validates the GitLab webhook token.
// If AMADEUS_GITLAB_WEBHOOK_SECRET is set, only that secret is accepted.
// Otherwise, it checks the active secret and, if the overlap window has not expired,
// the previous secret stored in the database (encrypted using KEK).
func (s *Service) ValidateWebhookToken(ctx context.Context, token string) bool {
	// ctEq is a local alias for constant-time string comparison to prevent
	// timing-based webhook secret enumeration (PP-L10).
	ctEq := func(a, b string) bool {
		return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
	}

	// 1. Env var override check
	if envSecret := os.Getenv("AMADEUS_GITLAB_WEBHOOK_SECRET"); envSecret != "" {
		return ctEq(token, envSecret)
	}

	// 2. Read secrets from database
	row := s.db.QueryRowContext(ctx, `
		SELECT webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until
		FROM gitlab_config WHERE id=1`)
	var secretEnc, prevEnc, overlapUntil sql.NullString
	if err := row.Scan(&secretEnc, &prevEnc, &overlapUntil); err != nil {
		// Fallback to static webhookSecret loaded at boot if DB query fails or has no rows
		return s.webhookSecret != "" && ctEq(token, s.webhookSecret)
	}

	// 3. Decrypt and check active secret
	if secretEnc.Valid && secretEnc.String != "" && s.Cfg != nil {
		decrypted, err := secrets.DecryptString(s.Cfg, secretEnc.String)
		if err == nil && ctEq(token, decrypted) {
			return true
		}
	} else if s.webhookSecret != "" && ctEq(token, s.webhookSecret) {
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
}

// TriggerSync starts an async sync and returns immediately. Overlapping calls are
// coalesced — if a sync is already running, this is a no-op.
func (s *Service) TriggerSync(ctx context.Context, triggeredBy string) {
	s.mu.Lock()
	if s.syncing {
		s.mu.Unlock()
		s.logInfo("sync already in progress — coalescing", "trigger", triggeredBy)
		return
	}
	s.syncing = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.syncing = false
			s.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				s.logError("git sync panicked", "panic", r)
			}
		}()
		res := s.sync(context.Background(), triggeredBy)
		if res.Status != "success" {
			// GitLab is an optional dependency that degrades gracefully (T12):
			// an unconfigured/unreachable remote is a warning, not an error.
			if s.repoURL == "" {
				s.logWarn("git sync skipped — GitLab not configured", "status", res.Status)
			} else {
				s.logError("git sync failed", "status", res.Status, "error", res.ErrorMessage)
			}
		} else {
			s.logInfo("git sync complete",
				"sha", res.SHA,
				"jobs", res.JobsSynced,
				"scripts", res.ScriptsSynced,
				"schedules", res.SchedulesSynced,
				"workflows", res.WfsSynced,
				"scopes", res.ScopesSynced)
		}
	}()
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

// logWarn logs at Warn level, safely handling a nil logger.
func (s *Service) logWarn(msg string, args ...any) {
	if s.log != nil {
		s.log.Warn(msg, args...)
	}
}

// SyncBlocking runs a sync synchronously and returns the result. Used by the
// operator-triggered POST /api/v1/git/sync.
func (s *Service) SyncBlocking(ctx context.Context, triggeredBy string) SyncResult {
	return s.sync(ctx, triggeredBy)
}

// sync is the internal sync implementation.
func (s *Service) sync(ctx context.Context, triggeredBy string) SyncResult {
	start := time.Now().UTC()
	res := SyncResult{Status: "failed", StartedAt: start}
	if s.db == nil {
		res.ErrorMessage = "db not configured"
		res.FinishedAt = time.Now().UTC()
		return res
	}

	branch := s.writeBranch(ctx)
	repo, err := s.cloneOrFetch(branch)
	if err != nil {
		res.ErrorMessage = err.Error()
		res.FinishedAt = time.Now().UTC()
		_ = s.recordSyncEvent(ctx, triggeredBy, res)
		return res
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
	// never-blocking posture as the advisory columns). `amadeus validate` / CI
	// turns the identical findings into hard errors.
	for _, sc := range scripts {
		if strings.TrimSpace(sc.Spec.ProjectRoot) == "" {
			continue
		}
		for _, f := range LintProject(s.cloneDir, sc.Spec.ProjectRoot) {
			s.log.Warn("git sync: checkout project lint (advisory)",
				"project", sc.Spec.ProjectRoot, "file", f.File, "detail", f.Message)
		}
	}

	// Resolve first-class schedules (compute content hashes) so jobs/workflows can
	// expand their scheduleRefs into the runtime definition_schedules cache (A10a).
	resolvedScheds, schedResolveErrs := s.resolveSchedules(scheds)
	collect(schedResolveErrs)
	schedsOK = schedsOK && len(schedResolveErrs) == 0

	// dangleScheduleRefs reports any scheduleRef that resolves to no schedule, as a
	// hard sync error (caught at sync/MR time, not run time — same as script_ref).
	dangleScheduleRefs := func(file string, refs []string) bool {
		bad := false
		for _, ref := range refs {
			if _, ok := resolvedScheds[ref]; !ok {
				ve := ValidationError{File: file, Field: "spec.scheduleRefs",
					Message: fmt.Sprintf("scheduleRef %q does not resolve to any schedules/*.yaml", ref)}
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
	// onOutcome enum, non-negative delays) and is ALSO run by `amadeus validate`
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
	// validation rule, it is a race. Only an `amadeus`-source upstream (an in-app
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
			if kind == "workflow" {
				return incomingWfs[name]
			}
			return incomingJobs[name]
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
			if _, ok := resolved[j.Spec.ScriptRef]; !ok {
				ve := ValidationError{File: "jobs/" + j.Metadata.Name + ".yaml", Field: "spec.script_ref",
					Message: fmt.Sprintf("script_ref %q does not resolve to any script in scripts/", j.Spec.ScriptRef)}
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
		if uErr := s.upsertJobs(ctx, tx, goodJobs, resolved, resolvedScheds, nowStr, sha); uErr != nil {
			allErrs = append(allErrs, "upsert jobs: "+uErr.Error())
			dbErr = uErr
		} else {
			res.JobsSynced = len(goodJobs)
		}
	}

	if dbErr == nil {
		if uErr := s.upsertWorkflows(ctx, tx, goodWfs, resolvedScheds, nowStr, sha); uErr != nil {
			allErrs = append(allErrs, "upsert workflows: "+uErr.Error())
			dbErr = uErr
		} else {
			res.WfsSynced = len(goodWfs)
		}
	}

	if dbErr == nil {
		if uErr := s.upsertScopes(ctx, tx, scopes, nowStr, sha); uErr != nil {
			allErrs = append(allErrs, "upsert scopes: "+uErr.Error())
			dbErr = uErr
		} else {
			res.ScopesSynced = len(scopes)
		}
	}

	// GitOps Pruning (Item 1): delete rows no longer in Git. PP-B2 — each entity
	// is pruned ONLY when its subsystem parsed cleanly (xxxOK); a subsystem with
	// any parse/resolve/cross-ref error keeps its stale rows for this cycle rather
	// than wiping the catalog. prune() no-ops once dbErr is set so a real DB error
	// still halts the chain.
	prune := func(label, query string) int64 {
		if dbErr != nil {
			return 0
		}
		r, err := tx.ExecContext(ctx, query, nowStr)
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

	if jobsOK {
		prune("job schedules", `
			DELETE FROM definition_schedules
			WHERE owner_source = 'git' AND owner_kind = 'job' AND owner_name IN (
				SELECT name FROM jobs WHERE source = 'git' AND synced_at < ? AND source_path LIKE 'jobs/%'
			)`)
		prunedJobs = prune("jobs", `
			DELETE FROM jobs
			WHERE source = 'git' AND synced_at < ? AND source_path LIKE 'jobs/%'`)
		// PP-H9: clean paused_jobs orphaned by the prune above so a re-added
		// same-named git job doesn't silently inherit a stale pause. Migration 200's
		// AFTER DELETE trigger already does this per-row; this is order-independent
		// defense-in-depth that survives a future trigger regression. Kept INSIDE
		// the jobsOK gate so a parse-skipped subsystem keeps its valid pauses.
		pruneNoArg("orphaned job pauses", `
			DELETE FROM paused_jobs
			WHERE owner_kind = 'job' AND source = 'git'
			  AND name NOT IN (SELECT name FROM jobs WHERE source = 'git')`)
	} else {
		skipped = append(skipped, "jobs")
	}

	if wfsOK {
		prune("workflow schedules", `
			DELETE FROM definition_schedules
			WHERE owner_source = 'git' AND owner_kind = 'workflow' AND owner_name IN (
				SELECT name FROM workflows WHERE source = 'git' AND synced_at < ? AND source_path LIKE 'workflows/%'
			)`)
		prunedWfs = prune("workflows", `
			DELETE FROM workflows
			WHERE source = 'git' AND synced_at < ? AND source_path LIKE 'workflows/%'`)
		// PP-H9: workflow analog of the paused_jobs orphan sweep above.
		pruneNoArg("orphaned workflow pauses", `
			DELETE FROM paused_jobs
			WHERE owner_kind = 'workflow' AND source = 'git'
			  AND name NOT IN (SELECT name FROM workflows WHERE source = 'git')`)
	} else {
		skipped = append(skipped, "workflows")
	}

	if scriptsOK {
		prunedScripts = prune("scripts", `
			DELETE FROM scripts
			WHERE synced_at < ? AND source_path LIKE 'scripts/%'`)
	} else {
		skipped = append(skipped, "scripts")
	}

	// git-source schedules only (source='amadeus' rows are operator-authored and
	// untouched — same guard as scopes; A9/§5.6).
	if schedsOK {
		prune("schedules", `
			DELETE FROM schedules
			WHERE source = 'git' AND synced_at < ?`)
	} else {
		skipped = append(skipped, "schedules")
	}

	if scopesOK {
		prune("scope hosts", `
			DELETE FROM scope_hosts
			WHERE scope_id IN (
				SELECT id FROM scopes WHERE synced_at < ? AND source = 'git'
			)`)
		// M4 — reap imported (git) ssh_hosts rows dropped from inventory this sync.
		// Only source='git' rows; operator (amadeus) overlays are never touched.
		prune("imported ssh hosts", `
			DELETE FROM ssh_hosts
			WHERE source = 'git' AND (synced_at IS NULL OR synced_at < ?)`)
		prunedScopes = prune("scopes", `
			DELETE FROM scopes
			WHERE synced_at < ? AND source = 'git'`)
	} else {
		skipped = append(skipped, "scopes")
	}

	// PP-B2 (B2-3): a skipped prune leaves stale rows — surface it. The parse
	// errors are already in allErrs (status → "partial"); this names the affected
	// subsystems for the operator.
	if len(skipped) > 0 {
		s.log.Warn("git sync: prune skipped for subsystems with parse errors — stale rows retained this cycle",
			"subsystems", skipped)
	}
	// PP-B2 (B2-4): a full wipe of a kind (rows deleted while zero synced) is the
	// signature of an accidental empty/rewritten tree — log it loudly even when the
	// parse looked clean, so an operator can catch a bad force-push.
	if (prunedJobs > 0 && res.JobsSynced == 0) || (prunedWfs > 0 && res.WfsSynced == 0) ||
		(prunedScripts > 0 && res.ScriptsSynced == 0) || (prunedScopes > 0 && res.ScopesSynced == 0) {
		s.log.Warn("git sync: a definition kind was fully pruned (0 synced, rows deleted) — verify the repo was not mid-rewrite",
			"jobs_pruned", prunedJobs, "workflows_pruned", prunedWfs,
			"scripts_pruned", prunedScripts, "scopes_pruned", prunedScopes)
	}

	if dbErr == nil {
		// Update git_sync_state.
		_, dbErr = tx.ExecContext(ctx,
			`INSERT INTO git_sync_state(id, last_sha, last_synced_at, last_status)
	         VALUES(1, ?, ?, ?)
	         ON CONFLICT(id) DO UPDATE SET
	             last_sha=excluded.last_sha,
	             last_synced_at=excluded.last_synced_at,
	             last_status=excluded.last_status`,
			sha, nowStr, func() string {
				if len(allErrs) == 0 {
					return "success"
				}
				return "partial"
			}())
		if dbErr != nil {
			allErrs = append(allErrs, "update sync state: "+dbErr.Error())
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
	// callback self-gates on SHA change, so a no-op poll (same SHA) is cheap.
	if s.onSyncComplete != nil {
		s.onSyncComplete(ctx, sha)
	}
	return res
}

// writeBranch resolves the working branch used for both sync (read) and publish
// (write): env override → DB gitlab_config.write_branch → "main". Read fresh per
// operation so a settings change takes effect without a restart (V1.1-10).
func (s *Service) writeBranch(ctx context.Context) string {
	if s.Cfg != nil && s.Cfg.GitLabWriteBranch != "" {
		return s.Cfg.GitLabWriteBranch
	}
	if s.db != nil {
		var b sql.NullString
		if err := s.db.QueryRowContext(ctx,
			`SELECT write_branch FROM gitlab_config WHERE id=1`).Scan(&b); err == nil && b.Valid && b.String != "" {
			return b.String
		}
	}
	return "main"
}

// cloneOrFetch clones the repo if not present; otherwise fetches and hard-resets.
// After either path it initializes/updates any git submodules (best-effort) so
// files contributed by a submodule — e.g. an Ansible playbooks repo mounted at
// scripts/playbooks — are present in the working tree for script discovery.
func (s *Service) cloneOrFetch(branch string) (*gogit.Repository, error) {
	if s.repoURL == "" {
		return nil, fmt.Errorf("AMADEUS_GITLAB_BASE_URL not configured")
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
		fetchOpts := &gogit.FetchOptions{RemoteName: "origin", Force: true}
		if auth != nil {
			fetchOpts.Auth = auth
		}
		ferr := repo.Fetch(fetchOpts)
		if ferr != nil && ferr != gogit.NoErrAlreadyUpToDate {
			return nil, fmt.Errorf("git fetch: %w", ferr)
		}
		// Hard-reset to origin/<branch>.
		remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
		if err != nil {
			return nil, fmt.Errorf("resolve origin/%s: %w", branch, err)
		}
		if err := wt.Reset(&gogit.ResetOptions{
			Commit: remoteRef.Hash(),
			Mode:   gogit.HardReset,
		}); err != nil {
			return nil, fmt.Errorf("git reset --hard: %w", err)
		}
		// Pull any submodules up to their pinned commits so their files are
		// present for discovery (e.g. scripts/playbooks). Best-effort.
		s.updateSubmodules(repo, wt, auth)
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
	repo, err = gogit.PlainClone(s.cloneDir, false, cloneOpts)
	if err != nil {
		return nil, fmt.Errorf("git clone %s: %w", s.repoURL, err)
	}
	// Initialize + check out submodules so their files are present for discovery.
	// RecurseSubmodules is intentionally NOT set on CloneOptions above so that a
	// submodule fetch failure cannot fail the whole clone — updateSubmodules runs
	// it as a separate best-effort step instead.
	if wt, werr := repo.Worktree(); werr == nil {
		s.updateSubmodules(repo, wt, auth)
	}
	return repo, nil
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
func (s *Service) updateSubmodules(repo *gogit.Repository, wt *gogit.Worktree, auth *gogithttp.BasicAuth) {
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
		if err := sub.Update(opts); err != nil {
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

// walkManifests recursively collects every .yaml/.yml file under dir so jobs,
// workflows, and schedules can live in sub-folders (folder support). A missing
// dir is not an error; a read failure is recorded but never aborts the walk,
// mirroring discoverScripts' advisory model.
func walkManifests(dir string) ([]manifestFile, []error) {
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
		data, rerr := os.ReadFile(p)
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
	files, errs := walkManifests(dir)
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
	return discoverScripts(dir)
}

// ──────────────────────────────────────────────────────────────────────────────
// Parse schedules/*.yaml (A10a — first-class Schedules)
// ──────────────────────────────────────────────────────────────────────────────

func (s *Service) parseSchedules() ([]ScheduleYAML, []error) {
	dir := filepath.Join(s.cloneDir, "schedules")
	files, errs := walkManifests(dir)
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
	files, errs := walkManifests(dir)
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
// Parse inventory/*.ini (+ optional *.amadeus.yaml sidecars)
// ──────────────────────────────────────────────────────────────────────────────

// inventoryScope is the parsed representation of a single inventory file.
type inventoryScope struct {
	Name        string
	SourcePath  string
	Content     string // byte-exact raw inventory (the -i material; persisted to scopes.raw_inventory)
	Format      string // "ini" (only .ini is parsed today)
	Capability  ScopeCapability
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".amadeus.yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var sc sidecarYAML
		if err := yaml.Unmarshal(data, &sc); err != nil {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".amadeus.yaml")
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
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
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

		cap := resolveInventoryCapability(content, sidecar, sidecarPath)
		for _, ce := range cap.Errors {
			errs = append(errs, ce)
		}

		desc := ""
		if sidecar != nil && sidecar.Spec.Description != "" {
			desc = sidecar.Spec.Description
		} else {
			directives, _ := parseAmadeusPragma(content)
			desc = directives.Description
		}

		scopes = append(scopes, inventoryScope{
			Name:          base,
			SourcePath:    "inventory/" + e.Name(),
			Content:       content,
			Format:        "ini",
			Capability:    cap,
			SidecarPath:   sidecarPath,
			Hosts:         parseInventoryHosts(content),
			Description:   desc,
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
	runType     string
	command     string
	script      string
	scriptPath  string
	executor    string
	description string
	contentHash string
	sourcePath  string
	projectRoot string // §7 checkout project: repo-relative dir, "" for a plain script
	warnings    string // JSON array of body-lint Warnings ("[]" when clean)
	variables   string // JSON array of referenced env Variables ("[]" when none)
	prompts     string // JR-Q6 — JSON array of DECLARED run inputs ("[]" when none)
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
			errs = append(errs, ValidationError{File: sc.SourcePath,
				Message: "compute content hash: " + err.Error()})
			continue
		}
		out[name] = resolvedScript{
			runType:     sc.Spec.RunType,
			command:     sc.Spec.Command,
			script:      sc.Spec.Script,
			scriptPath:  sc.Spec.ScriptPath,
			executor:    sc.Spec.Executor,
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

// upsertScripts writes the resolved scripts into the scripts cache table.
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
			INSERT INTO scripts(name, description, run_type, command, script, script_path,
			                    executor, content_hash, source_path, synced_at, warnings, variables,
			                    project_root, prompts_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET
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
			name, nullStr(rs.description), rs.runType,
			nullStr(rs.command), nullStr(rs.script), nullStr(rs.scriptPath),
			nullStr(rs.executor), rs.contentHash, nullStr(rs.sourcePath), now, warnings, variables,
			nullStr(rs.projectRoot), prompts)
		if err != nil {
			return fmt.Errorf("upsert script %q: %w", name, err)
		}
	}
	return nil
}

// resolvedSchedule is a first-class Schedule with its content hash computed (A10a).
type resolvedSchedule struct {
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
// operator-authored (source='amadeus') schedule computes the identical digest a
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
// cache table as source='git' rows (operator 'amadeus' rows are left untouched).
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
		_, err := tx.ExecContext(ctx, `
			INSERT INTO schedules(name, source, description, cron, env, content_hash, source_path, synced_at, start_at, end_at, interval, skip_calendars, only_calendars, uid)
			VALUES(?, 'git', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source, name) DO UPDATE SET
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
			db.NewID())
		if err != nil {
			return fmt.Errorf("upsert schedule %q: %w", name, err)
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
			Name: ref, Cron: rs.cron, Env: rs.env, SourceRef: ref,
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
// tags. YAML `spec.tags` is parsed-but-unused (kept on the spec struct). The amadeus
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
		command, script, scriptPath, executor := j.Spec.Command, j.Spec.Script, j.Spec.ScriptPath, j.Spec.Executor
		var scriptRef, contentHash, projectRoot string
		if j.Spec.ScriptRef != "" {
			rs := resolved[j.Spec.ScriptRef]
			runType = rs.runType
			command, script, scriptPath, executor = rs.command, rs.script, rs.scriptPath, rs.executor
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
		if sshCred != "" {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM ssh_credentials WHERE label = ? LIMIT 1`, sshCred).Scan(&one); err != nil {
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
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM secrets WHERE key = ? LIMIT 1`, bp).Scan(&one); err != nil {
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
				s.log.Warn("job declares an unusable file watch; ignoring it", "job", name, "problem", ve)
			}
			watches = nil
		}
		watchJSON := watchspec.Marshal(watches)
		warnDeadline := strings.TrimSpace(j.Spec.MustFinishBy)
		if warnDeadline != "" && !cronutil.ValidDeadline(warnDeadline) {
			s.log.Warn("job declares an unparseable must_finish_by; ignoring it",
				"job", name, "value", warnDeadline)
			warnDeadline = ""
		}
		// RT-2 — the declared runner pin, advisory-warned like the two above: an
		// unusable value leaves the job unpinned rather than failing the repo's sync.
		// Deliberately NOT validated against the live fleet (RT-Q4): git is often
		// where a tag appears first, and a runner enrolled an hour later must find
		// the job already asking for it.
		declaredRunnerTag := NormalizeRunnerTag(j.Spec.RunnerTag)
		if j.Spec.RunnerTag != "" && declaredRunnerTag == "" {
			// logWarn, not s.log.Warn: s.log is nil in the upsert-level tests, and
			// the neighbouring warns in this function are split between the two
			// spellings. A diagnostic must never be the thing that panics a sync.
			s.logWarn("git sync: job declares an unusable runner_tag; ignoring it (the job stays unpinned)",
				"job", name, "value", j.Spec.RunnerTag)
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
			                 warn_after_seconds, must_finish_by, watch_json, runner_tag, uid)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(source, name) WHERE source = 'git' DO UPDATE SET
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
				watch_json=excluded.watch_json,
				-- RT-2: the DECLARED pin is git-owned and sync-OVERWRITTEN, the
				-- opposite of jobs.tags above. Omitting this line would make
				-- spec.runner_tag parse and silently do nothing.
				-- TestRunnerTagSyncsFromYAML guards this direction.
				runner_tag=excluded.runner_tag`,
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
			// RT-2 — the declared runner pin. Normalized through the same tag rules
			// the runner-tag editor uses, so a YAML tag and a fleet tag that differ
			// only in case or whitespace still match at claim time. An unparseable
			// value degrades to unpinned rather than failing the sync, matching the
			// advisory-warn precedent set by must_finish_by and become_password above.
			nullStr(declaredRunnerTag),
			// AF-4a — see the schedules upsert: first-sight identity, preserved on
			// conflict by omission from DO UPDATE.
			db.NewID())
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
		// uid is authoritative here (the git pool is name-unique by constraint).
		var jobUID string
		if err := tx.QueryRowContext(ctx,
			`SELECT uid FROM jobs WHERE source='git' AND name = ?`, name).Scan(&jobUID); err != nil {
			return fmt.Errorf("resolve uid for job %q: %w", name, err)
		}
		// LU-6 §5.6.1 — a prune/return cycle mints a FRESH uid (first-sight, the
		// tags rule) but must NOT mint a fresh log folder: re-point the surviving
		// live registry row at the new identity before allocating, so Allocate
		// finds it instead of opening a new era.
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity_codes SET uid = ? WHERE kind = ? AND source = 'git' AND name = ? AND deleted_at IS NULL`,
			jobUID, entitycode.KindJob, name); err != nil {
			return fmt.Errorf("re-point entity code for job %q: %w", name, err)
		}
		if _, err := entitycode.Allocate(ctx, tx, entitycode.KindJob, "git", name, jobUID); err != nil {
			return fmt.Errorf("allocate entity code for job %q: %w", name, err)
		}
		if err := writeDefinitionReactions(ctx, tx, "git", "job", name, j.Spec.Reactions); err != nil {
			return fmt.Errorf("write reactions for job %q: %w", name, err)
		}
		if err := writeDefinitionSchedules(ctx, tx, "git", "job", name, entries); err != nil {
			return fmt.Errorf("write schedules for job %q: %w", name, err)
		}
	}
	return nil
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
func writeDefinitionReactions(ctx context.Context, tx *sql.Tx, ownerSource, ownerKind, ownerName string, entries []ReactionEntry) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM reactions WHERE owner_source = ? AND owner_kind = ? AND owner_name = ?`,
		ownerSource, ownerKind, ownerName); err != nil {
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
				CASE ?
				  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
				  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
				END,
				CASE ?
				  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
				  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
				END)`,
			ownerSource, ownerKind, ownerName, e.Name,
			e.OnSourceOrDefault(), e.OnKind, e.OnName, e.OnOutcome,
			e.DelaySeconds, e.MinIntervalSeconds, children, enabled, i,
			ownerKind, ownerName, ownerSource, ownerName, ownerSource,
			e.OnKind, e.OnName, e.OnSourceOrDefault(), e.OnName, e.OnSourceOrDefault()); err != nil {
			return err
		}
	}
	return nil
}

func writeDefinitionSchedules(ctx context.Context, tx *sql.Tx, ownerSource, ownerKind, ownerName string, entries []ScheduleEntry) error {
	// Source-scoped replace (A9): only this owner's (source-qualified) rows are
	// cleared, so a git sync can never wipe an operator's amadeus bindings.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM definition_schedules WHERE owner_source = ? AND owner_kind = ? AND owner_name = ?`,
		ownerSource, ownerKind, ownerName); err != nil {
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
				CASE ?
				  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
				  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
				END,
				(SELECT s.uid FROM schedules s WHERE s.name = ?
				   AND (SELECT COUNT(*) FROM schedules s2 WHERE s2.name = s.name) = 1))`,
			ownerSource, ownerKind, ownerName, e.Name, e.Cron, envJSON, i, nullStr(e.SourceRef),
			nullStr(e.StartAt), nullStr(e.EndAt), nullStr(e.Interval),
			nullStr(calendar.MarshalNames(e.SkipCalendars)), nullStr(calendar.MarshalNames(e.OnlyCalendars)),
			ownerKind, ownerName, ownerSource, ownerName, ownerSource,
			nullStr(e.SourceRef)); err != nil {
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
				s.log.Warn("git workflow has a structural problem; syncing it anyway",
					"workflow", name, "step", verr.Step, "field", verr.Field, "problem", verr.Message)
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
			INSERT INTO workflows(name, description, steps, schedule, enabled, source_path, synced_at, uid)
			VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(source, name) WHERE source = 'git' DO UPDATE SET
				description=excluded.description,
				steps=excluded.steps,
				schedule=excluded.schedule,
				enabled=excluded.enabled,
				source_path=excluded.source_path,
				synced_at=excluded.synced_at`,
			name, wf.Spec.Description, string(stepsJSON),
			nullStr(legacyMirror), enabled,
			defPath(wf.SourcePath, "workflows/"+name+".yaml"), now,
			db.NewID())
		if err != nil {
			return fmt.Errorf("upsert workflow %q: %w", name, err)
		}
		// LU-6: see upsertJobs. Same idempotent allocation, workflow kind.
		var wfUID string
		if err := tx.QueryRowContext(ctx,
			`SELECT uid FROM workflows WHERE source='git' AND name = ?`, name).Scan(&wfUID); err != nil {
			return fmt.Errorf("resolve uid for workflow %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity_codes SET uid = ? WHERE kind = ? AND source = 'git' AND name = ? AND deleted_at IS NULL`,
			wfUID, entitycode.KindWorkflow, name); err != nil {
			return fmt.Errorf("re-point entity code for workflow %q: %w", name, err)
		}
		if _, err := entitycode.Allocate(ctx, tx, entitycode.KindWorkflow, "git", name, wfUID); err != nil {
			return fmt.Errorf("allocate entity code for workflow %q: %w", name, err)
		}
		if err := writeDefinitionReactions(ctx, tx, "git", "workflow", name, wf.Spec.Reactions); err != nil {
			return fmt.Errorf("write reactions for workflow %q: %w", name, err)
		}
		if err := writeDefinitionSchedules(ctx, tx, "git", "workflow", name, entries); err != nil {
			return fmt.Errorf("write schedules for workflow %q: %w", name, err)
		}
	}
	return nil
}

func (s *Service) upsertScopes(ctx context.Context, tx *sql.Tx, scopes []inventoryScope, now string, sha string) error {
	// Scopes table is owned by B6 (settings) but B3 writes git-source rows.
	// Check if the table exists before writing to avoid failing when B6 hasn't
	// been applied yet (test isolation). In production all migrations run before
	// any handler is called.
	var exists int
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='scopes'`).Scan(&exists)
	if exists == 0 {
		return nil // scopes table not yet created — no-op
	}

	for _, sc := range scopes {
		// OD-1: a git inventory must NEVER clobber an operator-authored (amadeus)
		// scope of the same name. The ON CONFLICT(name) upsert below would otherwise
		// flip its source to 'git' and overwrite its authored raw/projection/hosts —
		// a silent hijack. Skip the colliding git scope loudly; the operator renames
		// one. (Names are unique, so an amadeus row owning the name blocks the git one.)
		var existingSource string
		_ = tx.QueryRowContext(ctx, `SELECT source FROM scopes WHERE name=?`, sc.Name).Scan(&existingSource)
		if existingSource == "amadeus" {
			s.logWarn("git inventory name collides with an amadeus-authored scope — skipping (rename one)", "name", sc.Name)
			continue
		}
		typesJSON, _ := json.Marshal(sc.Capability.Types)
		capJSON, _ := json.Marshal(map[string]any{
			"types":       sc.Capability.Types,
			"origin":      string(sc.Capability.Origin),
			"owner":       sc.Capability.Owner,
			"sidecarPath": sc.Capability.SidecarPath,
			"errors":      sc.Capability.Errors,
		})
		id := db.NewID()
		// agency_id is OPERATOR-OWNED and intentionally absent from both the INSERT
		// column list and the ON CONFLICT DO UPDATE SET below (agency-support.md M1,
		// §3.2 / D-OWN) — so an operator's agency binding survives re-sync, the same
		// operator-owned model as tags. Do NOT add agency_id here.
		//
		// The SAME RULE now covers the scope_agencies join table (migration 670,
		// the agencies plan AG-Q6/T2.9), which supersedes agency_id in
		// Phase 3: **git sync must neither INSERT nor DELETE scope_agencies rows.**
		// A sync that "reconciled" them would silently drop operator-assigned
		// membership on every pull — and because this upsert is ON CONFLICT on
		// scopes(name), it runs on every scope on every sync, so the damage would be
		// total and immediate. There is deliberately no membership reconciliation
		// anywhere in this file; TestSyncPreservesScopeAgencies pins that.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO scopes(id, name, source, source_path, capability_types, capability_json, synced_at, description, created_by, created_at, supported_types)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET
				source=excluded.source,
				source_path=excluded.source_path,
				capability_types=excluded.capability_types,
				capability_json=excluded.capability_json,
				synced_at=excluded.synced_at,
				description=excluded.description,
				supported_types=excluded.supported_types`,
			id, sc.Name, "git", sc.SourcePath, string(typesJSON), string(capJSON), now, sc.Description, "gitlab", now, string(typesJSON))
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

		// M1: persist the byte-exact raw inventory + format so an amadeus-mode
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
	return nil
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
// inventory can't be reliably imported. The upsert leaves host_key/status/
// last_checked_at UNTOUCHED so a TOFU-captured key survives re-sync (§9.4), and
// stamps synced_at=now so the prune-by-owner can reap rows dropped from inventory.
// Best-effort: log-and-continue if the 400 schema isn't present (isolated tests).
func (s *Service) importGitHosts(ctx context.Context, tx *sql.Tx, scopeID string, sc inventoryScope, now string) {
	if sc.Projection.PreviewUnavailable {
		// The inventory DEGRADED — we can't re-parse connection vars, but the hosts
		// are still in the file. A projection degrade is NOT a scopeErr, so the
		// prune-by-owner still runs; re-stamp synced_at on this scope's EXISTING git
		// rows so the prune leaves them, preserving their last-good connection detail
		// AND any TOFU-captured host_key (OD-13/§9.4 — a captured key must survive a
		// transient degrade, e.g. a host-range line or one bad [group:vars]).
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
func (s *Service) recordSyncEvent(ctx context.Context, triggeredBy string, res SyncResult) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO git_sync_events(triggered_by, sha, status, jobs_synced, scripts_synced, schedules_synced, wfs_synced, scopes_synced, error_message, started_at, finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
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

	var repoPath, branch string
	_ = s.db.QueryRow(`SELECT project_path, branch FROM gitlab_config WHERE id=1`).Scan(&repoPath, &branch)
	if repoPath == "" {
		repoPath = "infra/job-defs"
	}
	if branch == "" {
		branch = "main"
	}

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

// GetSyncState reads the current git_sync_state row.
func (s *Service) GetSyncState(ctx context.Context) (SyncState, error) {
	var st SyncState
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_sha,''), COALESCE(last_synced_at,''), COALESCE(last_status,'') FROM git_sync_state WHERE id=1`).
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
