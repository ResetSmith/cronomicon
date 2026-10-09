// Package sshexec is the local runner's engine (execution-update.md EX.4;
// 2.3.0, LR-40): an opt-in, in-process worker pool that executes shell runs
// over SSH (direct or via a bastion) against a single host or a scope of
// hosts. It has no claim of its own: it claims through runner.Claim, as the
// local runner's row, so agencies, scope bindings and requirements apply to
// the server as they do to an agent. It shares the runner subsystem's
// redaction seam, kill mechanism (action_queue), concurrency cap, and
// metrics/notify terminal seam — an engine, not a parallel lifecycle.
package sshexec

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/remotecmd"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"golang.org/x/crypto/ssh"
)

// target aliases the shared resolution type so this package's existing
// references read unchanged; resolution lives in internal/execspec (the single
// source of truth shared with the future runner-manifest endpoint — §3).
type target = execspec.Target

// SupportedRunType reports whether the SSH executor can run the given run-type.
// Thin re-export of execspec.SupportedRunType so existing call sites
// (internal/api/execution_mount.go) and tests keep referencing sshexec.
func SupportedRunType(runType string) bool { return execspec.SupportedRunType(runType) }

// Service is the SSH executor worker pool.
type Service struct {
	db          *sql.DB
	cfg         *config.Config
	log         *slog.Logger
	sec         *secrets.Service
	resolver    *runref.Resolver // dispatch-time reference injection (P1.3)
	notifier    *notify.Dispatcher
	gitCacheDir string
	// logDir is held atomically because the operator can re-point the run-log
	// directory while the executor is running (LU-5), and openLog reads it on
	// every claimed run.
	logDir      atomic.Pointer[string]
	concurrency int
	fanout      int

	// probeMu serializes "Test connection" probes per target (key
	// "host:<id>" / "bastion:<id>") so double-clicks and concurrent operators
	// can't stack parallel dials at the same host (ssh-update.md TC.2).
	probeMu sync.Map

	// The claim loop's lifecycle (Phase A of the local runner). The engine is
	// started and stopped at runtime by the `localRunner.enabled` setting
	// (Apply), so the loop has a context of its own: cancelling it stops
	// CLAIMING, and the runs already claimed go on to finish under the process
	// context (base). stopLoop is nil while the loop is not running. startErr
	// is why Start could not set the local runner up, when it could not: Apply
	// then reports it instead of answering "applied" for an engine that cannot
	// run.
	runMu    sync.Mutex
	base     context.Context
	stopLoop context.CancelFunc
	localID  string
	startErr error

	// How many runs it takes at once. The bound and the count of runs in
	// flight belong to the SERVICE, not to a loop: a loop that is stopped
	// leaves its runs finishing, and the loop started after it must count them
	// (off then on with four long runs in flight is still four, not eight).
	// claimMu makes "is there a free slot" and "claim one" a single step, so
	// two loop generations that overlap for an instant cannot both take the
	// last slot. slotFree wakes a waiting loop when a run finishes.
	bound    atomic.Int64
	inflight atomic.Int64
	claimMu  sync.Mutex
	slotFree chan struct{}

	// run executes one claimed run. It is s.execute everywhere but in the
	// tests of the loop itself, which need a run that stays in flight.
	run func(ctx context.Context, r claimedRun)

	// shutdownWG, when set, tracks the claim loop, the orphan reaper, and every
	// in-flight run goroutine so graceful shutdown can wait for them to finalize
	// before the DB pool closes (PP-L15) — preventing a finalize/UPDATE from
	// racing a closing pool and leaving a ghost.
	shutdownWG *sync.WaitGroup
}

// WithShutdownWG registers a WaitGroup that Start's goroutines (claim loop,
// orphan reaper, in-flight runs) join, so the process can drain them on
// shutdown before closing the DB pool (PP-L15). Chainable; returns s.
func (s *Service) WithShutdownWG(wg *sync.WaitGroup) *Service {
	s.shutdownWG = wg
	// The terminal-notify dispatch reads the shared pool asynchronously; track it
	// in the same WaitGroup so a shutdown drain covers it too (PP-L15).
	if s.notifier != nil {
		s.notifier.WithShutdownWG(wg)
	}
	return s
}

// track wraps fn in the shutdown WaitGroup when one is set. Add() is called
// synchronously (before the goroutine) so it can't race a concurrent Wait().
func (s *Service) track(fn func()) {
	if s.shutdownWG != nil {
		s.shutdownWG.Go(fn)
		return
	}
	go fn()
}

// New builds the executor. gitCacheDir is where scriptPath jobs are read from;
// logDir is the per-run log directory (shared with the runner subsystem).
func New(db *sql.DB, cfg *config.Config, log *slog.Logger, gitCacheDir, logDir string) *Service {
	conc := cfg.SSHExecutorConcurrency
	if conc <= 0 {
		conc = 4
	}
	// Wire the app's configured Vault client so vault-source secrets AND SSH
	// credentials (P2.4) resolve through this executor's resolver/loadSigner — the
	// same client the API server uses. Unconfigured ⇒ the stub, unchanged behavior.
	sec := settings.WireVaultClient(context.Background(), db, cfg, secrets.New(db, cfg, log), log)
	svc := &Service{
		db:          db,
		cfg:         cfg,
		log:         log,
		sec:         sec,
		resolver:    runref.NewResolver(db, cfg, sec, log),
		notifier:    notify.New(db, cfg, log),
		gitCacheDir: gitCacheDir,
		concurrency: conc,
		fanout:      4, // EX-D5 bounded-parallel within a run
		slotFree:    make(chan struct{}, 1),
	}
	svc.run = svc.execute
	svc.SetLogDir(logDir)
	return svc
}

// SetLogDir points the executor at a run-log directory. Safe to call while runs
// are in flight: an open logSink keeps writing to the handle it already has, and
// the next claimed run lands in the new directory (LU-5).
func (s *Service) SetLogDir(dir string) {
	s.logDir.Store(&dir)
}

// LogDir returns the current run-log directory.
func (s *Service) LogDir() string {
	if p := s.logDir.Load(); p != nil {
		return *p
	}
	return ""
}

// Start brings the engine up: it reconciles crash-orphaned runs, makes sure the
// local runner's row exists, and starts claiming if the local runner is turned
// on. Off is the default — a deploy that never turns it on behaves as before
// (ssh runs queue, never execute).
func (s *Service) Start(ctx context.Context) {
	// Reconcile crash-orphaned runs BEFORE anything else, even when the local
	// runner is off: a run of the server's still 'running' at process start has
	// no live worker and permanently consumes a global concurrency slot until
	// reconciled (PP-H2). Synchronous so a fast restart frees slots before the
	// loop begins claiming. Safe under the single-instance invariant.
	s.sweepOrphansOnStartup(ctx)

	// The row always exists (LR-39): bindings, placements and ledger rows key
	// on its id. Its switch and its concurrency are seeded ONCE from the names
	// the SSH executor had until 2.3.0 (LR-44, LR-45), so an upgrade arrives in
	// the state it was running in.
	id, seeded, err := settings.EnsureLocalRunner(ctx, s.db, s.cfg.SSHExecutorEnabled, s.cfg.SSHExecutorConcurrency)
	if err != nil {
		s.log.Error("local runner: could not set up its row; it stays off", "error", err)
		s.runMu.Lock()
		s.startErr = fmt.Errorf("the local runner could not be set up when the server started: %w", err)
		s.runMu.Unlock()
		return
	}
	if seeded {
		s.log.Info("local runner: first start — its on/off setting was taken from CRONOMICON_SSH_EXECUTOR_ENABLED; "+
			"from now on it is changed in Settings, and CRONOMICON_LOCAL_RUNNER=forbid turns it off for good",
			"enabled", s.cfg.SSHExecutorEnabled)
	}
	s.runMu.Lock()
	s.base, s.localID = ctx, id
	s.runMu.Unlock()

	// Once, on the first start of 2.3.0: put the local runner where the server
	// was serving, record how each scope's shell jobs were served, and move
	// the rows still queued for the SSH executor (settings.RunLocalRunnerUpgradePass).
	// Before Apply, so the loop's first claim already sees the placements. A
	// failure is logged and tried again at the next start: nothing was marked
	// done.
	// Once, likewise: the host keys this server had captured on first connect
	// become the local runner's approved keys (runner.CarryServerHostKeys), so
	// every host it was connecting to goes on being connected to now that it
	// verifies against its ledger and captures nothing.
	if _, cerr := runner.CarryServerHostKeys(ctx, s.db, s.log); cerr != nil {
		s.log.Error("local runner: could not carry the server's host keys into its ledger; "+
			"hosts it has no approved key for will fail host_key_unverified until this succeeds at a later start", "error", cerr)
	}

	wasOn, werr := settings.SSHExecutorWasOn(ctx, s.db)
	if werr != nil {
		s.log.Error("local runner: could not read what the SSH executor was for the upgrade pass", "error", werr)
	} else if _, uerr := settings.RunLocalRunnerUpgradePass(ctx, s.db, s.log, wasOn); uerr != nil {
		s.log.Error("local runner: the 2.3.0 upgrade pass failed; it will be tried again at the next start", "error", uerr)
	}

	// The periodic orphan reaper runs whether or not the local runner is on: a
	// run it started can outlive the switch being turned off.
	s.track(func() { s.reapOrphansLoop(ctx) })
	if err := s.Apply(ctx); err != nil {
		s.log.Error("local runner: could not read its setting; it stays off", "error", err)
	}
}

// Apply brings the engine in line with the stored switch and concurrency: it
// starts the claim loop, stops it, or restarts it with a new bound. It is
// called at start and after every change of the setting (the API's PUT
// /local-runner), so a change takes effect without a restart.
//
// Turning off DRAINS (LR-43): the loop stops claiming, and the runs it has
// already claimed finish. Nothing is failed, and nothing goes through the
// runner reaper's offline path, which would fail them as lost.
//
// A new bound needs no restart: the loop reads it before every claim. Lowering
// it below what is in flight fails nothing; the loop claims again once enough
// runs have finished.
//
// ctx bounds the reads and writes this call makes. The loop it starts lives
// under the process context given to Start, never under ctx: ctx is a request's,
// and the loop must outlive the request that turned it on.
func (s *Service) Apply(ctx context.Context) error { //nolint:contextcheck // see above: the loop is the process's, not the caller's
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.startErr != nil {
		return s.startErr
	}
	if s.base == nil || s.localID == "" {
		return nil // never started (the probe-only instance): nothing to apply
	}
	// Read under the lock. Two changes applied at once must each act on what
	// is stored when its turn comes, or the later reader can undo the earlier
	// writer and leave the engine the opposite of the setting.
	enabled, err := settings.LocalRunnerEnabled(ctx, s.db, s.cfg.LocalRunnerForbid)
	if err != nil {
		return err
	}
	conc := s.concurrency
	if err := s.db.QueryRowContext(ctx, `SELECT max_concurrent FROM runners WHERE id = ?`, s.localID).Scan(&conc); err != nil {
		return err
	}
	if conc <= 0 {
		conc = 4
	}
	s.bound.Store(int64(conc))
	running := s.stopLoop != nil
	switch {
	case enabled && !running:
		loopCtx, cancel := context.WithCancel(s.base)
		s.stopLoop = cancel
		s.setLocalStatus(ctx, "online")
		s.log.Warn("local runner ON — this process holds SSH private keys and has outbound SSH to job targets",
			"concurrency", conc)
		base := s.base
		s.track(func() { s.loop(loopCtx, base) })
		s.track(func() { s.heartbeat(loopCtx) })
	case enabled:
		// Already claiming; the new bound is in force from the next claim.
	case running:
		s.stopLoop()
		s.stopLoop = nil
		s.setLocalStatus(ctx, "offline")
		s.log.Info("local runner OFF — it claims no more; runs it is running finish",
			"in_flight", s.inflight.Load())
	default:
		s.setLocalStatus(ctx, "offline")
		if s.cfg.LocalRunnerForbid {
			s.log.Info("local runner forbidden on this host (CRONOMICON_LOCAL_RUNNER=forbid) — ssh runs will queue")
		} else {
			s.log.Info("local runner off — ssh runs will queue until it is turned on in Settings")
		}
	}
	return nil
}

// setLocalStatus writes the local runner's status. Its row says "online" only
// while the claim loop is running; the heartbeat keeps last_seen_at fresh so
// the Runners view reads it as it reads an agent.
func (s *Service) setLocalStatus(ctx context.Context, status string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET status = ?, last_seen_at = CASE WHEN ? = 'online' THEN ? ELSE last_seen_at END WHERE id = ?`,
		status, status, ts, s.localID); err != nil {
		s.log.Error("local runner: write status", "status", status, "error", err)
	}
}

// heartbeatEvery is how often the local runner restates that it is on.
var heartbeatEvery = 20 * time.Second

// heartbeat keeps the local runner's row fresh while it is on. It is only a
// freshness signal for the Runners view: the runner reaper skips the local
// runner altogether, so a stalled heartbeat can never fail its runs.
//
// It restates the status as well as the time: the write Apply made when it
// turned the runner on may have failed, and this is what puts the row right.
// It writes under runMu and only while its loop's context is live, so a beat
// that fires as the runner is turned off cannot land "online" after Apply
// wrote "offline".
func (s *Service) heartbeat(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runMu.Lock()
			if ctx.Err() == nil {
				s.setLocalStatus(ctx, "online")
			}
			s.runMu.Unlock()
		}
	}
}

// loop claims and runs. ctx is the CLAIM loop's context: cancelling it stops
// claiming. runCtx is the process context: a run that was claimed executes
// under it, so turning the local runner off lets it finish.
func (s *Service) loop(ctx, runCtx context.Context) {
	for {
		run, err := s.claimSlot(ctx, runCtx)
		if ctx.Err() != nil && run == nil {
			return
		}
		if err != nil {
			s.log.Error("ssh executor claim", "error", err)
		}
		if run == nil {
			// Nothing to claim, no free slot, or a failed claim: wait for a
			// run to finish or for the next look at the queue.
			select {
			case <-ctx.Done():
				return
			case <-s.slotFree:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		r := *run
		runFn := func() {
			defer func() {
				s.inflight.Add(-1)
				select {
				case s.slotFree <- struct{}{}:
				default:
				}
			}()
			defer func() {
				if rec := recover(); rec != nil {
					s.log.Error("ssh executor panic", "trace_id", r.traceID, "panic", rec)
					s.finalize(context.Background(), r, "failure", nil)
				}
			}()
			s.run(runCtx, r)
		}
		// Track in-flight runs in the shutdown WaitGroup (PP-L15) so a graceful
		// stop waits for them to finalize before the pool closes.
		s.track(runFn)
	}
}

// claimSlot claims one run if the local runner has a free slot, and counts it
// as in flight before it returns. (nil, nil) means there was no slot, nothing
// queued, or the loop has been stopped.
//
// The claim itself runs under the PROCESS context, not the loop's. The loop's
// context is cancelled when the runner is turned off, and a statement cancelled
// as it completes can report an error for an UPDATE that was applied: the run
// would be 'running' with no worker, holding its concurrency key until the
// stale-run reaper found it a day later. So the loop's context is consulted
// before the claim, and never interrupts one.
func (s *Service) claimSlot(ctx, runCtx context.Context) (*claimedRun, error) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()
	if ctx.Err() != nil || s.inflight.Load() >= s.bound.Load() {
		return nil, nil
	}
	run, err := s.claim(runCtx)
	if err != nil {
		if runCtx.Err() != nil {
			return nil, nil // shutting down
		}
		return nil, err
	}
	if run != nil {
		s.inflight.Add(1)
	}
	return run, nil
}

type claimedRun struct {
	traceID      string
	jobName      string
	jobSource    string // git | cronomicon (A9); resolves the job's denormalized body by (name,source)
	scriptRef    string // referenced Script name as authored; "" ⇒ legacy inline body
	scriptUID    string // WHICH script that name meant when the run was queued (runs.script_uid, 1290); keys the script's bindings
	jobUID       string // the executed job's frozen identity (runs.job_uid, R2-1); keys the binding read (R2F-1)
	runType      string
	scope        string
	targetHost   string
	triggeredBy  string    // actor that triggered the run (→ CRONOMICON_RUN_TRIGGERED_BY)
	envJSON      string    // schedule env snapshot (JSON object string); injected at exec
	overrideJSON string    // F3 ad-hoc override envelope (hosts/groups/bindings; env values are log-visible)
	entityCode   string    // LU-7 log folder, stamped at enqueue; "" ⇒ the pre-710 flat layout
	sshUser      string    // CA — frozen per-run "connect as" login; "" ⇒ per-host resolution
	sshCred      string    // CA — frozen per-run ssh_credentials LABEL; "" ⇒ per-host resolution
	startedAt    time.Time // claim instant; basis for duration_ms written at finalize (LB1)
}

// claim atomically transitions the oldest queued ssh-executor run to running.
func (s *Service) claim(ctx context.Context) (*claimedRun, error) {
	caps, err := s.localCaps(ctx)
	if err != nil {
		return nil, err
	}
	// The one claim (LR-40): the statement an agent's poll runs, for this
	// server's own row. Agencies, scope bindings, requirements and priority
	// apply to the local runner exactly as they do to an agent; the SSH pool's
	// own query, which knew none of them, is gone. Secret injection is always
	// allowed (LR-46: this process is the secret store), and the one clause
	// that is the local runner's alone keeps it from a run that binds a key.
	c, err := runner.Claim(ctx, s.db, s.log, runner.ClaimRequest{
		RunnerID:    s.localID,
		Caps:        caps,
		InjectionOK: true,
		Local:       true,
		Actor:       localRunnerActor,
	})
	if err != nil || c == nil {
		return nil, err
	}
	r := claimedRun{traceID: c.TraceID, jobName: c.JobName, runType: c.RunType, scope: c.Scope}
	if t, perr := time.Parse(time.RFC3339, c.StartedAt); perr == nil {
		r.startedAt = t
	} else {
		r.startedAt = time.Now().UTC()
	}
	metrics.RunStarted()

	// The rest of the row, by id. The run is already 'running' and this
	// process is its only worker, so a failed read must not return an error
	// and walk away: it is finalized here, with the cause.
	var targetHost, envJSON, overrideJSON, jobSource, jobUID, scriptRef, scriptUID, triggeredBy, entityCode, sshUser, sshCred sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT job_source, job_uid, script_ref, script_uid, target_host, triggered_by, env_json, override_json,
		       entity_code, ssh_user, ssh_credential
		  FROM runs WHERE id = ?`, c.TraceID).
		Scan(&jobSource, &jobUID, &scriptRef, &scriptUID, &targetHost, &triggeredBy, &envJSON, &overrideJSON,
			&entityCode, &sshUser, &sshCred); err != nil {
		s.log.Error("local runner: claimed a run and could not read it; failing it", "trace_id", c.TraceID, "error", err)
		s.finalizeReason(ctx, r, "failure", nil, "local_runner_read_failed")
		return nil, nil
	}
	r.jobSource = jobSource.String
	r.jobUID = jobUID.String
	r.scriptRef = scriptRef.String
	r.scriptUID = scriptUID.String
	r.targetHost = targetHost.String
	r.triggeredBy = triggeredBy.String
	r.envJSON = envJSON.String
	r.overrideJSON = overrideJSON.String
	r.entityCode = entityCode.String
	r.sshUser = sshUser.String
	r.sshCred = sshCred.String
	return &r, nil
}

// localRunnerActor is the activity actor of a run the local runner took
// (LR-53). Rows written before 2.3.0 say "ssh-executor" and are left as they are.
const localRunnerActor = "local-runner"

// localCaps is what the local runner may claim: the capabilities on its row
// (the four shell types, LR-41) minus the operator's mask, as for an agent.
func (s *Service) localCaps(ctx context.Context) ([]string, error) {
	var capsJSON string
	var managed sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(capabilities, '[]'), managed_settings FROM runners WHERE id = ?`, s.localID).
		Scan(&capsJSON, &managed); err != nil {
		return nil, fmt.Errorf("read the local runner: %w", err)
	}
	var caps []string
	_ = json.Unmarshal([]byte(capsJSON), &caps)
	var ms struct {
		CapabilityMask []string `json:"capabilityMask"`
	}
	if managed.Valid && managed.String != "" {
		_ = json.Unmarshal([]byte(managed.String), &ms)
	}
	return execspec.EffectiveCaps(caps, ms.CapabilityMask), nil
}

// execute resolves the command + targets, fans out over SSH, and finalizes.
func (s *Service) execute(ctx context.Context, r claimedRun) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.watchKill(runCtx, cancel, r.traceID)

	// Resolve the run's declared references BEFORE opening the log so the log
	// redactor is seeded with the injected secret values (and key material). A
	// nil resolved ⇒ injection is off (kill-switch, which intentionally reverts
	// the WHOLE v0.49.x injection behavior including CRONOMICON_RUN_* context) or
	// resolution failed (rerr).
	var resolved *runref.Resolved
	var rerr error
	if s.cfg.SecretsInjectionEnabled {
		resolved, rerr = s.resolveReferences(runCtx, r)
	}
	var extraRedact []string
	if resolved != nil {
		extraRedact = resolved.Redact
	}

	sink, err := s.openLog(runCtx, r, extraRedact)
	if err != nil {
		// Fail-closed: openLog returns an error when it holds injected secret values
		// but cannot build the redactor — running would risk those values surfacing
		// un-masked. Report failure without a sink (the cause is in the server log).
		s.log.Error("ssh executor open log", "trace_id", r.traceID, "error", err)
		s.finalize(ctx, r, "failure", nil)
		return
	}
	defer sink.close()

	// Fail-closed: a missing / out-of-scope / un-revealable binding halts the run
	// rather than executing it without the secret it declared. M2: the precise error
	// (out-of-scope vs missing) goes to the SERVER log only; the operator-visible run
	// log gets runref.OperatorMessage — a single generic message for both — so a run
	// cannot be used as a cross-scope existence oracle. The message names references
	// only (never values).
	if rerr != nil {
		s.log.Error("ssh executor resolve references", "trace_id", r.traceID, "error", rerr)
		sink.line("", "cronomicon: reference injection failed: "+runref.OperatorMessage(rerr))
		s.finalize(ctx, r, "failure", nil)
		return
	}
	// Dispatch audit (P1.6): record WHICH references were injected — names + source,
	// never values — to change_log before executing, and fail closed if it can't be
	// recorded (a run must not inject references with no audit trail; mirrors the
	// reveal handler and the runner manifest path).
	if resolved != nil && len(resolved.Refs) > 0 {
		if err := s.auditInjection(runCtx, r, resolved); err != nil {
			s.log.Error("ssh executor injection audit failed", "trace_id", r.traceID, "error", err)
			sink.line("", "cronomicon: dispatch audit failed; run halted (references are not injected without an audit trail)")
			s.finalize(ctx, r, "failure", nil)
			return
		}
	}

	// LR-47 — a bound SSH key that reached the local runner. The claim keeps it
	// away from a job or script that declares a key binding, so this is
	// reachable only by a binding the claim cannot see: one that resolves from
	// the run's own overrides. The local runner connects FROM the server and
	// cannot place a key on the target, so the run fails HERE, before
	// connecting — after the injection audit above, in the same order the
	// missing-binding path uses (the audit records what was declared).
	if resolved != nil && len(resolved.Keys) > 0 {
		names := make([]string, 0, len(resolved.Keys))
		for _, k := range resolved.Keys {
			names = append(names, k.Reference)
		}
		s.log.Warn("local runner: a key-bound run reached it",
			"trace_id", r.traceID, "references", names)
		sink.line("", "cronomicon: this run binds SSH key "+strings.Join(names, ", ")+
			" and was taken by the local runner, which cannot deliver key files; it needs an agent — or bind the key as a Secret and write the file in the job body")
		s.finalize(ctx, r, "failure", nil)
		return
	}

	interp, body, err := execspec.ResolveCommand(runCtx, s.db, s.gitCacheDir, r.jobName, r.jobSource, r.runType)
	if err != nil {
		sink.line("", "cronomicon: "+err.Error())
		s.finalize(ctx, r, "failure", nil)
		return
	}

	// M3 — ResolveRun expands target groups to member hosts and unions them with
	// the explicit host subset; the SSH executor uses only the expanded targets
	// (it discards the --limit, which is the ansible runner's mechanism). Groups
	// arrive pre-expanded as ordinary Targets — no fan-out change.
	targets, _, err := execspec.ResolveRun(runCtx, s.db, r.scope, r.targetHost,
		execspec.OverrideHosts(r.overrideJSON), execspec.OverrideGroups(r.overrideJSON))
	if err != nil {
		sink.line("", "cronomicon: target resolution failed: "+err.Error())
		s.finalize(ctx, r, "failure", nil)
		return
	}
	if len(targets) == 0 {
		sink.line("", "cronomicon: no hosts resolved for this run (empty scope/target)")
		s.finalize(ctx, r, "failure", nil)
		return
	}

	// CA — apply the run's frozen "connect as" identity over every resolved
	// target. The stored value is a LABEL (CA-Q2); resolve it to the credential
	// id here, failing the run loudly if the credential was deleted/renamed
	// between enqueue and dispatch (same posture as a missing declared binding).
	if r.sshUser != "" || r.sshCred != "" {
		credID := ""
		if r.sshCred != "" {
			// LR-73: by the label AS THIS RUN'S AGENCY SEES IT — its own key of
			// that label before Global's, and never another agency's. Labels are
			// unique per owner, not across them, and this was `WHERE label = ?
			// LIMIT 1` over every agency: whichever row the database returned
			// first, with the agency checked only later, on the wrong row.
			credAgencies, aerr := runref.RunAgencies(runCtx, s.db, r.traceID)
			if aerr != nil {
				sink.line("", "cronomicon: could not read this run's agencies: "+aerr.Error())
				s.finalize(ctx, r, "failure", nil)
				return
			}
			id, found, err := runref.LookupEntityID(runCtx, s.db, runref.KindKey, r.sshCred, "", credAgencies)
			if err != nil {
				sink.line("", "cronomicon: ssh credential lookup failed: "+err.Error())
				s.finalize(ctx, r, "failure", nil)
				return
			}
			if !found {
				sink.line("", fmt.Sprintf("cronomicon: ssh credential %q is not one this run's agency may use (it was deleted or renamed since this run was queued, or belongs to another agency)", r.sshCred))
				s.finalize(ctx, r, "failure", nil)
				return
			}
			credID = id
		}
		targets = execspec.ApplyIdentityOverride(targets, r.sshUser, credID, "")
	}

	// A12/§13.2 — enforce the job's timeout so a hung or non-emitting step can't
	// run forever (previously TimeoutSeconds was shipped but never enforced).
	execCtx := runCtx
	if to := s.jobTimeout(runCtx, r.jobName, r.jobSource); to > 0 {
		var tcancel context.CancelFunc
		execCtx, tcancel = context.WithTimeout(runCtx, to)
		defer tcancel()
	}

	// (The SU-4 interim guard stood here: it refused to inject secrets over a
	// bastion to a target whose key had only been captured on first connect.
	// Nothing is captured on first connect any more. A target the local runner
	// has no approved key for is not connected to, with or without secrets.)

	// Every key this run connects with must be one its agency may use (see
	// keyGuard). The agencies are the run's FROZEN snapshot, as for reference
	// injection, and are read here unconditionally: this is an isolation check,
	// not part of the secrets-injection switch.
	runAgencies, aerr := runref.RunAgencies(runCtx, s.db, r.traceID)
	if aerr != nil {
		sink.line("", "cronomicon: could not read this run's agencies: "+aerr.Error())
		s.finalize(ctx, r, "failure", nil)
		return
	}
	guard := keyGuard{checked: true, scope: r.scope, agencies: runAgencies}

	cmd := remoteCommand(interp, body, r.envJSON, s.injectedEnv(r, resolved))
	results := s.fanOut(execCtx, targets, cmd, sink, guard)

	// H2/DEC-2 parity with the runner log-ingest path (runner/log.go): an
	// ::cronomicon-output:: value that carries an injected secret would propagate that
	// secret VERBATIM into outputs_json, into a child step's plaintext env_json
	// (workflow engine), and into the run-detail API — via the natural idiom
	// `echo "::cronomicon-output name=TOKEN::$CRONOMICON_SECRET_FOO"`. Fail the run closed
	// at this earliest choke point (before outputs_json is written): drop the
	// captured outputs and finalize the run failed so nothing propagates. The
	// offending value is already masked in the persisted log (the sink redactor is
	// seeded with the injected dictionary, extraRedact); we surface only the NAME.
	capturedOutputs := sink.snapshotOutputs()
	if leaked := execspec.FirstOutputLeakingSecret(capturedOutputs, extraRedact); leaked != "" {
		s.log.Error("ssh executor: captured output would leak an injected secret; failing run closed",
			"trace_id", r.traceID, "output", leaked)
		sink.line("", fmt.Sprintf(
			"cronomicon: output %q would leak an injected secret value; refusing to capture it and failing the run", leaked))
		s.finalizeReason(ctx, r, "failure", nil, "output_secret_leak")
		return
	}

	// A12 — persist captured inter-job outputs before flipping the run terminal.
	s.writeOutputs(ctx, r.traceID, capturedOutputs)

	status, exit := aggregate(results)
	if execCtx.Err() == context.DeadlineExceeded {
		sink.line("", "cronomicon: job timed out")
		status = "failure"
	}
	// A run that failed because the local runner has no approved key for a
	// host says so where History shows it, as an agent's run does, and not
	// only in its log: it is the one failure with a fixed remedy, and since
	// 2.3.0 it is what a host the server has never connected to produces.
	reason := ""
	if status != "success" {
		var unverified []string
		for _, hr := range results {
			if hr.err != nil && strings.Contains(hr.err.Error(), HostKeyUnverified) {
				unverified = append(unverified, hr.host)
			}
		}
		if len(unverified) > 0 {
			reason = HostKeyUnverified + ": " + strings.Join(unverified, ", ")
		}
	}
	s.finalizeReason(ctx, r, status, exit, reason)
}

// resolveReferences loads the run's declared reference bindings (from its job and,
// when it references one, its script) and resolves them to injectable values
// (vault-integration.md P1.3). Returns fail-closed on the first out-of-scope /
// missing / un-revealable binding.
//
// Scope: the SSH executor injects Secrets + Variables (values) + the CRONOMICON_RUN_*
// context (D7). A declared CRONOMICON_KEY_* reference resolves like any other
// binding — so its material enters the redaction dictionary and the audit row —
// and then FAILS the run in execute (KB): the key file would have to land on
// the TARGET host, which this executor cannot do. Producers refuse such a run at
// enqueue; the executor-side failure covers a binding added while it waited.
func (s *Service) resolveReferences(ctx context.Context, r claimedRun) (resolved *runref.Resolved, err error) {
	jobSource := r.jobSource
	if jobSource == "" {
		jobSource = "git" // mirror jobTimeout's normalization for the owner lookup
	}
	// R2F-1: the run carries its job's identity, so the bindings resolved here are
	// the ones the job that was ENQUEUED declared — not the union of every job
	// sharing its name.
	owners := []runref.Owner{{Kind: "job", Source: jobSource, Name: r.jobName, UID: r.jobUID}}
	if r.scriptUID != "" {
		owners = append(owners, runref.Owner{Kind: "script", Name: r.scriptRef, UID: r.scriptUID})
	}

	seen := map[string]bool{}
	var bindings []runref.Binding
	// The declared sets first, then the operator's per-run additions from the
	// override envelope (V2-11 — manual runs only; scheduled/workflow runs carry
	// no "references" entry). Additions ride the SAME loop so they get the same
	// dedupe as a declared reference.
	sets := make([][]runref.Binding, 0, len(owners)+1)
	for _, o := range owners {
		bs, lerr := runref.ListBindings(ctx, s.db, o)
		if lerr != nil {
			return nil, fmt.Errorf("load bindings: %w", lerr)
		}
		sets = append(sets, bs)
	}
	sets = append(sets, runref.OverrideBindings(r.overrideJSON))
	for _, bs := range sets {
		for _, b := range bs {
			// A job, its script and the per-run additions may all declare the same
			// reference; handle once. RA-4: the identity is kind+name+ALIAS, so one
			// row deliberately bound under two destinations survives as two bindings.
			key := runref.DedupeKey(b)
			if seen[key] {
				continue
			}
			seen[key] = true
			bindings = append(bindings, b)
		}
	}

	// actorScopes=nil: the run's scope was validated against the actor's grants at
	// enqueue (the HTTP execution handler), and no roles are stored on the run. The
	// resolver still enforces per-row scope against the run scope, so an out-of-scope
	// row still fails closed here.
	// AG-Q8 — the run's FROZEN agency snapshot (migration 680), not live membership:
	// a run's injectable set is determined by the world as it was when the run was
	// authorized, so re-homing a scope mid-flight cannot retarget it.
	runAgencies, aerr := runref.RunAgencies(ctx, s.db, r.traceID)
	if aerr != nil {
		return nil, aerr
	}
	return s.resolver.Resolve(ctx, nil, r.scope, runAgencies, bindings)
}

// auditInjection records this run's dispatch-time reference injection to change_log
// (P1.6), exactly once per run (guarded by runs.injection_audited). It records
// reference names + source only, NEVER values, and returns the WriteChangeLog error
// unmasked so execute() can fail closed. actor is the run's triggered_by (→ "system"
// when unattributed).
func (s *Service) auditInjection(ctx context.Context, r claimedRun, resolved *runref.Resolved) error {
	return runref.AuditInjectionOnce(ctx, s.db, s.log, r.traceID, r.triggeredBy, r.scope, resolved)
}

// injectedEnv is the dispatch-time env merged into the remote command: the fixed
// CRONOMICON_RUN_* run context plus the resolved reference values. Returns nil when
// injection is off (kill-switch) so the command is built exactly as before.
func (s *Service) injectedEnv(r claimedRun, resolved *runref.Resolved) map[string]string {
	if resolved == nil {
		return nil
	}
	out := runref.RunContext{
		ID:          r.traceID,
		Job:         r.jobName,
		JobSource:   r.jobSource,
		Scope:       r.scope,
		Type:        r.runType,
		TriggeredBy: r.triggeredBy,
		Executor:    "ssh",
	}.Env()
	// Reference values (CRONOMICON_SECRET_*/VAR_*) overlay the run context; the two
	// key-spaces are disjoint, so this only ever adds.
	maps.Copy(out, resolved.Env)
	return out
}

// jobTimeout reads the (source-qualified) job's timeout_seconds as a Duration (0 ⇒ none).
func (s *Service) jobTimeout(ctx context.Context, jobName, jobSource string) time.Duration {
	if jobSource == "" {
		jobSource = "git"
	}
	var secs sql.NullInt64
	_ = s.db.QueryRowContext(ctx,
		`SELECT timeout_seconds FROM jobs WHERE name = ? AND source = ?`, jobName, jobSource).Scan(&secs)
	if secs.Valid && secs.Int64 > 0 {
		return time.Duration(secs.Int64) * time.Second
	}
	return 0
}

// writeOutputs persists the A12 captured outputs as JSON on the run (no-op if empty).
func (s *Service) writeOutputs(ctx context.Context, traceID string, outputs map[string]string) {
	if len(outputs) == 0 {
		return
	}
	b, err := json.Marshal(outputs)
	if err != nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET outputs_json = ? WHERE id = ?`, string(b), traceID); err != nil {
		s.log.Error("ssh executor write outputs", "trace_id", traceID, "error", err)
	}
}

type hostResult struct {
	host     string
	exitCode int
	err      error
}

// fanOut runs the command across targets with bounded parallelism (EX-D5).
func (s *Service) fanOut(ctx context.Context, targets []target, cmd remotecmd.Rendered, sink *logSink, guard keyGuard) []hostResult {
	results := make([]hostResult, len(targets))
	sem := make(chan struct{}, s.fanout)
	var wg sync.WaitGroup
	for i, t := range targets {
		// Acquire OUTSIDE the goroutine: the semaphore is what bounds fan-out, so
		// taking the slot inside would let every target start at once.
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = s.runTarget(ctx, t, cmd, sink, guard)
		})
	}
	wg.Wait()
	return results
}

// runTarget connects to one host and runs the command, streaming output.
func (s *Service) runTarget(ctx context.Context, t target, cmd remotecmd.Rendered, sink *logSink, guard keyGuard) hostResult {
	if t.ResolveErr != "" {
		sink.line(t.Name, "cronomicon: "+t.ResolveErr)
		return hostResult{host: t.Name, exitCode: -1, err: fmt.Errorf("%s", t.ResolveErr)}
	}

	signer, err := loadSigner(ctx, s.db, s.cfg, s.sec, t.AuthCredentialID, t.AuthKeyEnvVar, guard)
	if err != nil {
		sink.line(t.Name, "cronomicon: auth: "+err.Error())
		return hostResult{host: t.Name, exitCode: -1, err: err}
	}

	client, closeFn, err := s.dial(ctx, t, signer)
	if err != nil {
		sink.line(t.Name, "cronomicon: connect: "+err.Error())
		return hostResult{host: t.Name, exitCode: -1, err: err}
	}
	defer closeFn()

	session, err := client.NewSession()
	if err != nil {
		sink.line(t.Name, "cronomicon: session: "+err.Error())
		return hostResult{host: t.Name, exitCode: -1, err: err}
	}
	defer session.Close()

	// PP-H1: session.Run below blocks until the remote command exits and takes no
	// context, so without this watcher neither the A12 job timeout (execCtx) nor
	// an operator kill (runCtx via watchKill's cancel) can interrupt a hung or
	// non-emitting command — the run would leak this goroutine + a concurrency
	// slot forever. Mirror the runner-agent sibling (agent/ssh.go): on
	// cancellation, SIGKILL + Close the session, which unblocks Run regardless of
	// whether the server honors signals. The done channel stops the watcher on
	// normal completion. (The watcher's Close + the defer above can both fire on
	// cancellation; the second Close is a harmless net.ErrClosed, ignored.)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-done:
		}
	}()

	stdout, _ := session.StdoutPipe()
	stderr, _ := session.StderrPipe()
	// H1: env (incl. injected secret values) is delivered on stdin, never argv.
	// Attach the stream only when there is one so a no-injection body still sees
	// EOF on stdin exactly as before.
	//
	// Written through an explicit StdinPipe rather than session.Stdin, and its
	// write error DELIBERATELY DISCARDED. With session.Stdin, x/crypto/ssh adds a
	// copyFunc whose error Wait() returns whenever the exit status itself was clean:
	//
	//	waitErr := <-s.exitStatus            // nil on a clean exit
	//	for range s.copyFuncs { … copyError … }
	//	if waitErr != nil { return waitErr }
	//	return copyError                     // ← a successful run, reported as an error
	//
	// So if the remote closed the session while we were still writing stdin, the
	// copy failed with io.EOF, which is not an *ssh.ExitError — the run scored -1,
	// emitted "cronomicon: EOF" and was marked FAILED even though the command exited 0.
	//
	// That is reachable in production, not just in tests. Since H1 the interpreter
	// is invoked in a form that reads its PROGRAM from stdin (`bash -s`, `python3 -`,
	// `perl`, `powershell -Command -`), and bash reads that program INCREMENTALLY —
	// a script with an early `exit 0` leaves the tail unread, and on a body large
	// enough to still be in flight the channel closes mid-write. Go's own os/exec
	// ignores exactly this class of error for exactly this reason (skipStdinCopyError,
	// issue 9173).
	//
	// A non-zero exit still surfaces normally: Wait() returns waitErr in preference
	// to any copy error. This discards only the case where the command succeeded by
	// its own account.
	if cmd.Stdin != "" {
		stdinPipe, perr := session.StdinPipe()
		if perr != nil {
			sink.line(t.Name, "cronomicon: stdin: "+perr.Error())
			return hostResult{host: t.Name, exitCode: -1, err: perr}
		}
		go func() {
			defer stdinPipe.Close()
			_, _ = io.WriteString(stdinPipe, cmd.Stdin)
		}()
	}
	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go func() { defer streamWG.Done(); scanLines(stdout, func(l string) { sink.line(t.Name, l) }) }()
	go func() { defer streamWG.Done(); scanLines(stderr, func(l string) { sink.line(t.Name, l) }) }()

	runErr := session.Run(cmd.Cmd)
	streamWG.Wait()

	exit := 0
	if runErr != nil {
		if ee, ok := runErr.(*ssh.ExitError); ok {
			exit = ee.ExitStatus()
		} else {
			exit = -1
			sink.line(t.Name, "cronomicon: "+runErr.Error())
		}
	}
	return hostResult{host: t.Name, exitCode: exit, err: runErr}
}

// aggregate folds per-host results into a terminal run status (EX-D5): all OK
// ⇒ success; all failed ⇒ failure; mixed ⇒ warning (partial).
func aggregate(results []hostResult) (string, *int) {
	ok, fail := 0, 0
	for _, r := range results {
		if r.exitCode == 0 && r.err == nil {
			ok++
		} else {
			fail++
		}
	}
	switch {
	case fail == 0:
		z := 0
		return "success", &z
	case ok == 0:
		o := 1
		return "failure", &o
	default:
		return "warning", nil
	}
}

// finalize writes the terminal run state and fires the shared metrics/notify
// seam — the same one the runner path uses (no duplication of policy).
func (s *Service) finalize(ctx context.Context, r claimedRun, status string, exit *int) {
	s.finalizeReason(ctx, r, status, exit, "")
}

// finalizeReason is finalize with an explicit terminal reason recorded in
// queued_reason (the same column the runner path uses as the terminal reason,
// e.g. output_secret_leak). An empty reason leaves any existing queued_reason
// untouched, so the plain finalize path is unchanged.
func (s *Service) finalizeReason(ctx context.Context, r claimedRun, status string, exit *int, reason string) {
	// Detach from cancellation: a run that was killed, timed out, or caught by
	// graceful shutdown (ctx cancelled) must STILL record its terminal status —
	// otherwise it lingers 'running' forever as an orphan that wedges the
	// concurrency cap (the very PP-H2 ghost the reaper exists to clean). Bound the
	// detached writes so a wedged DB can't block shutdown indefinitely.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	var exitVal any
	if exit != nil {
		exitVal = *exit
	}
	// Compute elapsed run time from the claim instant (LB1). Left NULL only if the
	// claim timestamp was never set (defensive — claim always populates it).
	var durVal any
	if !r.startedAt.IsZero() {
		durVal = now.Sub(r.startedAt).Milliseconds()
	}
	res, err := s.db.ExecContext(wctx, `
		UPDATE runs SET status = ?, completed_at = ?, duration_ms = ?, exit_code = ?,
		    queued_reason = COALESCE(NULLIF(?, ''), queued_reason)
		WHERE id = ? AND status = 'running'`, status, ts, durVal, exitVal, reason, r.traceID)
	if err != nil {
		s.log.Error("ssh executor finalize", "trace_id", r.traceID, "error", err)
	}
	// The slot is given back by whoever ends the run, once. A run the stale
	// reaper already reconciled is no longer 'running' here: its slot went
	// back then, and this worker finishing late must not give back another.
	if res != nil {
		if n, _ := res.RowsAffected(); n > 0 {
			s.releaseLoad(wctx)
		}
	}
	s.emitTerminal(wctx, r.traceID, r.jobName, r.scope, status, exit)
}

// releaseLoad gives back the slot the claim took on the local runner's row
// (runners.load, which the claim increments for every runner). An agent's is
// given back when its final log chunk arrives; the server's is given back here.
func (s *Service) releaseLoad(ctx context.Context) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET load = MAX(0, load - 1) WHERE kind = 'server'`); err != nil {
		s.log.Error("local runner: release load", "error", err)
	}
}

// emitTerminal fires the shared run-end seam — the activity row + RunFinished
// metric + notifier — for an SSH run. It's the SSH analogue of the runner's
// emitRunTerminal; both finalize() and the orphan reaper call it so the
// run-end side effects can't drift between the normal and reconcile paths.
// Callers own the runs terminal-status UPDATE (it differs: finalize records
// duration/exit, the reaper records queued_reason='executor_lost').
func (s *Service) emitTerminal(ctx context.Context, traceID, jobName, scope, status string, exit *int) {
	ts := time.Now().UTC().Format(time.RFC3339)
	_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		At:      ts,
		Kind:    "run-end",
		Outcome: status,
		Actor:   localRunnerActor,
		JobName: jobName,
		Scope:   scope,
		TraceID: traceID,
	})

	metrics.RunFinished(status)
	if s.notifier != nil {
		s.notifier.RunEnded(notify.RunEvent{
			TraceID: traceID, JobName: jobName, Scope: scope, Status: status, ExitCode: exit,
		})
	}
}

// sshReaperNow is the clock seam for the orphan reaper (mirrors runner reaper's
// reaperNow), overridable in tests so the age-based window is deterministic.
var sshReaperNow = func() time.Time { return time.Now().UTC() }

// orphan is a run left 'running' with no live worker (a crash/shutdown ghost).
type orphan struct {
	traceID   string
	jobName   string
	scope     string
	startedAt string
}

// reconcileOrphan transitions one orphaned ssh run to a terminal failure tagged
// queued_reason='executor_lost' and fires the shared run-end seam. The
// status='running' guard makes it idempotent (a run finalized in the meantime is
// left untouched and does not double-emit).
func (s *Service) reconcileOrphan(ctx context.Context, o orphan) bool {
	now := sshReaperNow()
	ts := now.Format(time.RFC3339)
	var durVal any
	if o.startedAt != "" {
		if started, err := time.Parse(time.RFC3339, o.startedAt); err == nil {
			if d := now.Sub(started).Milliseconds(); d > 0 { // guard clock skew (mirror runner reaper)
				durVal = d
			}
		}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status='failure', queued_reason='executor_lost', completed_at=?, duration_ms=?
		WHERE id=? AND status='running' AND `+localRunSQL, ts, durVal, o.traceID)
	if err != nil {
		s.log.Error("ssh executor reconcile orphan", "trace_id", o.traceID, "error", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false // already terminal — don't double-emit
	}
	s.releaseLoad(ctx)
	s.emitTerminal(ctx, o.traceID, o.jobName, o.scope, "failure", nil)
	return true
}

// localRunSQL is true of a run this process is the worker of: one the local
// runner claimed (its id on the row), or one the SSH executor claimed before
// 2.3.0, which stamped no runner and is known by its executor. The second arm
// is for rows a crash left 'running' across the upgrade; nothing writes
// executor='ssh' any more.
const localRunSQL = `(runner_id IN (SELECT id FROM runners WHERE kind = 'server') OR executor = 'ssh')`

// reconcileOrphans runs the given orphan query, reconciles each match, and
// returns the number reconciled. Rows are collected before any UPDATE so the
// read isn't invalidated mid-iteration.
func (s *Service) reconcileOrphans(ctx context.Context, query string, args ...any) int {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		s.log.Error("ssh executor orphan query", "error", err)
		return 0
	}
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.traceID, &o.jobName, &o.scope, &o.startedAt); err != nil {
			continue
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	n := 0
	for _, o := range orphans {
		if s.reconcileOrphan(ctx, o) {
			n++
		}
	}
	if n > 0 {
		metrics.SSHOrphansReconciled(n)
	}
	return n
}

// sweepOrphansOnStartup reconciles EVERY run of the server's (localRunSQL)
// still 'running' at process start. The engine is single-instance, so any such run is by
// definition orphaned (no live worker owns it — workers exist only within this
// process's loop). Running it synchronously before the claim loop frees the
// concurrency slots a crashed predecessor left wedged (PP-H2).
func (s *Service) sweepOrphansOnStartup(ctx context.Context) {
	n := s.reconcileOrphans(ctx, `
		SELECT id, job_name, COALESCE(scope,''), COALESCE(started_at,'')
		FROM runs WHERE status='running' AND `+localRunSQL)
	if n > 0 {
		s.log.Warn("local runner: reconciled orphaned runs at startup (executor_lost)", "count", n)
	}
	// Nothing of the local runner's is running now, and it is not claiming,
	// whatever a crash left on its row. Apply writes "online" when it starts
	// the loop; if the start fails before that, the row must not go on saying
	// a runner is there for the claim's mirrors to count.
	if _, err := s.db.ExecContext(ctx, `UPDATE runners SET load = 0, status = 'offline' WHERE kind = 'server'`); err != nil {
		s.log.Error("local runner: reset its row", "error", err)
	}
}

// reapOrphansLoop is the periodic safety net (mirrors runner StartReaper's
// goroutine lifecycle): every minute it reconciles the server's runs that have
// been 'running' longer than the stale window. The window is set far above any
// plausible job timeout so it never kills a live long-running run — the startup
// sweep, not this, is what recovers crashed runs quickly.
func (s *Service) reapOrphansLoop(ctx context.Context) {
	staleAfter := s.cfg.SSHExecutorStaleAfter
	if staleAfter <= 0 {
		staleAfter = 24 * time.Hour
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapOrphansOnce(ctx, staleAfter)
		}
	}
}

// reapOrphansOnce reconciles ssh runs 'running' past the stale window. Split out
// for deterministic testing via the sshReaperNow seam.
func (s *Service) reapOrphansOnce(ctx context.Context, staleAfter time.Duration) int {
	cutoff := sshReaperNow().Add(-staleAfter).Format(time.RFC3339)
	return s.reconcileOrphans(ctx, `
		SELECT id, job_name, COALESCE(scope,''), COALESCE(started_at,'')
		FROM runs WHERE status='running' AND `+localRunSQL+`
		  AND started_at IS NOT NULL AND started_at < ?`, cutoff)
}

// watchKill cancels the run context when an operator kill lands in action_queue
// (the kill button / endpoint already writes those rows).
func (s *Service) watchKill(ctx context.Context, cancel context.CancelFunc, traceID string) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var n int
			_ = s.db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM action_queue
				WHERE run_id = ? AND op = 'kill' AND consumed_at IS NULL`, traceID).Scan(&n)
			if n > 0 {
				_, _ = s.db.ExecContext(ctx, `
					UPDATE action_queue SET consumed_at = ?
					WHERE run_id = ? AND op = 'kill'`, time.Now().UTC().Format(time.RFC3339), traceID)
				s.log.Info("ssh executor: kill signal received", "trace_id", traceID)
				cancel()
				return
			}
		}
	}
}

// ── log sink: shared redaction + append (T7/S7), one writer per run ──────────

type logSink struct {
	mu       sync.Mutex
	f        *os.File
	redactor *runner.Redactor
	outputs  map[string]string // A12 captured inter-job outputs (raw values, for downstream injection)
}

func (s *Service) openLog(ctx context.Context, r claimedRun, extraRedact []string) (*logSink, error) {
	// LU-12: the trace ID comes from the runs row, but it reaches a filename here
	// with no shape check anywhere upstream. runner.LogPath validates both the
	// trace id and the entity code before joining them.
	dir := s.LogDir()
	path, perr := runner.LogPath(dir, r.entityCode, r.traceID)
	if perr != nil {
		return nil, fmt.Errorf("open log: %w", perr)
	}
	if err := runner.EnsureLogDir(dir, r.entityCode); err != nil {
		return nil, err
	}
	// LU-7: explain the folder on disk, once, at creation. Best-effort.
	runner.WriteEntityMeta(ctx, s.db, dir, r.entityCode)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	// Seed the redactor with the dispatch-time injected secret values / key
	// material (extraRedact, P1.3) so an injected secret can never surface
	// un-masked in this executor's logs. Run override env values (prompt
	// answers) are log-visible by design and are not fed in.
	red, err := runner.NewRedactor(ctx, s.db, s.cfg, r.scope, extraRedact...)
	if err != nil {
		// FAIL CLOSED when we are holding injected secret values that MUST be
		// masked: a broken redactor (e.g. a transient env_vars query error) would
		// let a freshly-dispatched secret echoed by the remote surface in cleartext
		// on the command line. Abort the run instead. With no injected secrets,
		// preserve the prior lenient behavior — scope-value masking is best-effort
		// and not P1.3's contract.
		if len(extraRedact) > 0 {
			f.Close()
			return nil, fmt.Errorf("redactor unavailable while injected secrets are present: %w", err)
		}
		red = nil
	}
	return &logSink{f: f, redactor: red, outputs: map[string]string{}}, nil
}

// line redacts and appends one log line, optionally prefixed with the host (for
// fan-out runs). Redaction runs through the SAME redactor the HTTP ingest uses.
//
// A12: an output marker is parsed from the RAW text (before redaction) so the
// captured value is exact; the line itself is still persisted redacted. On a
// scope fan-out, last host wins for a given key (single-target outputs are the
// supported case per Q-F).
func (l *logSink) line(host, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if k, v, ok := execspec.ParseOutputMarker(text); ok {
		l.outputs[k] = v
	}
	out := text
	if host != "" {
		out = "[" + host + "] " + text
	}
	b := []byte(out)
	if l.redactor != nil {
		b = l.redactor.Redact(b)
	}
	_, _ = l.f.Write(append(b, '\n'))
}

// snapshotOutputs returns a copy of the captured A12 outputs (nil if none).
func (l *logSink) snapshotOutputs() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.outputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(l.outputs))
	maps.Copy(out, l.outputs)
	return out
}

func (l *logSink) close() { _ = l.f.Close() }

// scanLines reads newline-delimited output and calls fn per line.
func scanLines(r interface{ Read([]byte) (int, error) }, fn func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fn(sc.Text())
	}
}
