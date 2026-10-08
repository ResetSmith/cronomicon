package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// agentVersion is the agent build version reported at registration. main.go can
// override it via SetVersion before Run.
var agentVersion = "dev"

// SetVersion sets the version string reported to the server at registration.
func SetVersion(v string) {
	if v != "" {
		agentVersion = v
	}
}

// Agent is the runner agent runtime: it owns the lifecycle loop (register →
// poll → execute → stream), bounded concurrency, and the kill/drain control
// channel. Per credential model (b) (D1) it never receives key bytes from the
// server — execution resolves keys from the agent's OWN local custody.
type Agent struct {
	cfg    Config
	client *Client
	log    *slog.Logger
	exec   *executor

	id Identity // populated by register/resume

	// configDigest is the canonical digest of the CURRENTLY declared config
	// (Phase 5 drift detection), sent with every poll so the server can spot
	// drift and request a re-register. Computed at startup (fresh register OR
	// resume — a resume after a config edit/upgrade is exactly the drift case)
	// and refreshed on redeclare. Only touched from the poll-loop goroutine.
	configDigest string

	// watcher polls file-arrival specs (ET-D). Non-nil only when the agent opted
	// in with -allow-watch AND supplied a -watch-paths allowlist, so an agent
	// that never asked for this costs nothing.
	watcher *watcher

	// active tracks per-trace cancel funcs so a kill control op can target the
	// matching run's execution context. len(active) is also the live run count
	// that bounds concurrency (checked under mu against the effective
	// maxConcurrent) — Phase 4 replaced a fixed-size channel semaphore with this
	// counter so the limit can be raised/lowered live by a managed setting.
	mu       sync.Mutex
	active   map[string]context.CancelCauseFunc
	draining bool
	wg       sync.WaitGroup

	// runParent is the parent of every run's context: the process context with
	// its cancellation removed (2.3.2). The signal that stops the agent ends
	// the claiming of work, not the work: a run ends when it finishes, when
	// the server kills it, or on Abort. Nil until Run sets it; dispatch then
	// falls back to the context it is given.
	runParent context.Context
	// idle receives when the last active run ends, so a stopping agent leaves
	// without waiting out its next heartbeat.
	idle chan struct{}
	// freed receives when a run ends on an agent that had no free slot, so the
	// agent asks for work again at once: while it was full its polls claimed
	// nothing (pollState.noClaim) and were not held by the server.
	freed chan struct{}

	// stopping (the process was signalled and is finishing its runs) and
	// answered (some poll of this process has had an answer) are what a poll
	// tells the server beyond being alive (pollState). Only touched from the
	// poll-loop goroutine.
	stopping bool
	answered bool

	// settings holds server-managed operational overrides applied in-memory
	// (Phase 4). Shared with a.exec so per-run code sees the same overrides.
	settings *settingsStore
}

// New builds an Agent from a resolved config. It constructs the HTTP client, the
// SSH runner (agent-local key custody), and loads the local inventory when in
// "local" mode.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	client, err := NewClient(cfg.ServerURL, cfg.CACertPath)
	if err != nil {
		return nil, err
	}
	// Apply the runtime defaults Resolve() deliberately leaves unset (the
	// known_hosts path beside the identity file) so every path — agent, doctor,
	// doctor --auth — dials against the SAME trust store.
	cfg = defaultedConfig(cfg)
	sshR := sshRunnerFor(cfg)
	var inv localInventory
	if cfg.Inventory == "local" {
		inv, err = loadLocalInventory(cfg.LocalInventoryFile)
		if err != nil {
			return nil, err
		}
	}
	store := &settingsStore{}
	return &Agent{
		cfg:      cfg,
		client:   client,
		log:      log,
		exec:     &executor{ssh: sshR, cfg: cfg, inv: inv, settings: store},
		active:   map[string]context.CancelCauseFunc{},
		idle:     make(chan struct{}, 1),
		freed:    make(chan struct{}, 1),
		settings: store,
	}, nil
}

// stopHeartbeat is how often a stopping agent polls while its runs finish. The
// server answers such a poll at once (it claims nothing for it), so this is
// also how long a kill sent in that time waits to be delivered.
const stopHeartbeat = 5 * time.Second

// registerTimeout bounds the registration exchange, which a stop signal does
// not interrupt (register).
const registerTimeout = 30 * time.Second

// errAgentStopped is the cause a run's context carries when Abort ended it.
var errAgentStopped = errors.New("the runner agent was stopped")

// Run drives the agent until ctx is cancelled AND its runs have finished. It
// registers (or resumes its identity), then loops polling at the configured
// cadence. It always keeps polling while runs execute (R2.2) so it stays
// heartbeat-fresh and receives kill/drain. On a 404 poll it re-registers (D4).
//
// Cancelling ctx (SIGTERM, SIGINT) is a drain, not a kill (2.3.2): the agent
// stops claiming, keeps its heartbeat, and returns when the runs in flight
// have ended. Until 2.3.2 every run's context was a child of ctx, so the
// signal killed them all and cancelled the upload of their logs with them,
// while the log line and the unit's comment both said "waiting for active runs
// to finish". Abort is the way to end them early.
func (a *Agent) Run(ctx context.Context) error {
	a.runParent = context.WithoutCancel(ctx)
	// Startup progress is logged step-by-step ON PURPOSE: each phase below can
	// block on the host (a $PATH dir on a hung mount stalls exec.LookPath, an
	// unreachable server stalls register), and the probes are best-effort. When
	// something wedges, the LAST line in the journal names the exact phase — no
	// goroutine dump required. Keep the "starting <phase>" logs immediately
	// BEFORE the call they describe.
	a.log.Info("runner agent starting",
		"version", agentVersion, "server", a.cfg.ServerURL, "name", a.cfg.Name)

	// Tier 2 sandbox availability probe (§6.3), once at startup. Skipped when the
	// sandbox is disabled (no point probing). Propagate to both the agent's cfg
	// (drives the advertised `sandboxed` token + toolchains) and the executor's
	// cfg copy (drives per-run wrapping).
	sb, sbWhy := false, ""
	if !a.cfg.NoSandbox {
		a.log.Info("probing tier-2 sandbox availability (systemd-run)")
		sb, sbWhy = probeSandbox(ctx)
	} else {
		a.log.Info("sandbox probe skipped (NoSandbox set)")
		sbWhy = "disabled (-no-sandbox)"
	}
	a.cfg.SandboxAvailable = sb
	a.exec.cfg.SandboxAvailable = sb
	if !sb {
		logNoSandbox(a.log, a.cfg.AllowCheckout, sbWhy)
	}

	if err := a.registerOrResume(ctx); err != nil {
		return err
	}
	// Resume path: register() computed the digest from its detected caps; a
	// RESUMED identity skipped detection, so probe now — this is the moment
	// drift detection exists for (config edited / binary upgraded + restart:
	// the first poll carries the new digest and the server requests the
	// re-declare automatically, Phase 5).
	if a.configDigest == "" {
		a.log.Info("detecting host capabilities for drift digest (resumed identity)")
		caps, _ := detectCapabilities(ctx, a.cfg)
		a.configDigest = a.declaredDigest(caps)
		// Loud on purpose: this detected set is what drift detection will make
		// the server adopt. If a toolchain probe failed transiently (e.g. the
		// agent started before ansible's PATH/venv was ready), the reduced
		// token set propagates — restart (or Resync) once the toolchain is
		// healthy to restore it.
		a.log.Info("declared-config digest computed from detected capabilities (drift detection)",
			"capabilities", caps)
	}
	a.log.Info("runner agent online",
		"id", a.id.ID, "name", a.cfg.Name, "capabilities", a.cfg.Capabilities,
		"inventory", a.cfg.Inventory, "poll_interval", a.cfg.PollInterval)

	// ET-D — start the file watcher once the agent is online, only if this host
	// opted in AND bounded itself. Its scan loop is independent of the poll loop:
	// a stability window measured in seconds should not be quantised to the poll
	// interval, which an operator may have set to minutes.
	if a.cfg.AllowWatch && len(a.cfg.WatchPaths) > 0 {
		a.watcher = newWatcher(a)
		a.log.Info("file-arrival watching enabled", "allowedRoots", a.cfg.WatchPaths)
		go a.watcher.run(ctx)
	} else if a.cfg.AllowWatch {
		// Opted in but unbounded: say so loudly rather than watching nothing in
		// silence, which would look identical to the server never sending specs.
		a.log.Warn("-allow-watch is set but -watch-paths is empty; nothing will be watched")
	}

	// Report the trust store once at startup: a known_hosts seeded on this host
	// by hand is otherwise invisible to the server until somebody asks.
	go a.reportKnownHosts(ctx)

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	// The poll in flight when the signal arrives is cancelled by it (it may be
	// held by the server for half a minute, and could come back with work).
	// The polls after it must not be: they are the heartbeat of the drain.
	for {
		if a.stopping {
			// Bounded: the server answers a stopping agent's poll at once, and an
			// unreachable server must not keep the agent after its last run ends.
			hb, cancel := context.WithTimeout(a.runParent, 2*stopHeartbeat)
			a.pollOnce(hb) //nolint:contextcheck // deliberate: the drain's heartbeat outlives the cancelled signal context
			cancel()
		} else {
			a.pollOnce(ctx)
		}

		if a.isDrainingDone() {
			a.log.Info("drain complete — no active runs, exiting")
			a.wg.Wait() // the goroutines of runs that have just left `active`
			return ctx.Err()
		}

		if a.stopping {
			wait := min(stopHeartbeat, a.cfg.PollInterval)
			select {
			case <-a.idle:
			case <-time.After(wait):
			}
			continue
		}

		select {
		case <-ctx.Done():
			n := a.startDrain("shutdown signal")
			if n == 0 {
				return ctx.Err()
			}
			a.log.Info("shutdown signal — claiming no new work and waiting for active runs to finish; "+
				"a second signal cancels them", "active_runs", n)
			a.stopping = true
		case <-ticker.C:
		case <-a.freed:
			// A slot opened on an agent that had none: ask for work now.
		}
	}
}

// full reports whether every concurrency slot is taken, by the same count and
// the same (possibly server-managed) limit that dispatch enforces.
func (a *Agent) full() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.active) >= a.settings.maxConcurrent(a.cfg.MaxConcurrent)
}

// refuse reports a run that the server assigned and this agent will not
// execute. The server has already moved the run to running, here; if the agent
// only logged the fact, as it did before 2.3.2, nothing would ever end the run.
// So it ends it: one line saying why, and the envelope of a failure. With
// pollState.noClaim this should not happen at all; it is what keeps the cases
// that remain (a limit or a mask that changed between the claim and now, a
// server older than the agent) from stranding a run.
func (a *Agent) refuse(ctx context.Context, asn *runnerproto.PollAssignment, why string) {
	a.log.Warn("assigned a run this agent will not execute — reporting it as failed", "trace_id", asn.TraceID, "reason", why)
	a.wg.Go(func() {
		flush, stop := context.WithTimeout(context.WithoutCancel(ctx), terminalFlushTimeout)
		defer stop()
		now := time.Now()
		buf := &logBuffer{}
		buf.writeLine("cronomicon: this runner was handed the run and did not start it: " + why + ". Nothing was executed; run it again.")
		buf.seal(makeEnvelope(1, now, now, ""))
		if err := streamLogs(flush, a.client, a.id, asn.TraceID, buf, a.cfg.LogRetryBudget, false); err != nil && !errors.Is(err, errRunClosed) {
			a.log.Error("could not report a refused assignment", "trace_id", asn.TraceID, "error", err)
		}
	})
}

// Abort cancels every run in flight. It is what a second stop signal does:
// the first one drains (Run), and an operator who will not wait sends another.
// The runs end as cancelled and still upload their logs, so the server records
// what happened to each instead of losing them.
func (a *Agent) Abort() {
	a.mu.Lock()
	cancels := make([]context.CancelCauseFunc, 0, len(a.active))
	for _, cancel := range a.active {
		cancels = append(cancels, cancel)
	}
	a.mu.Unlock()
	if len(cancels) > 0 {
		a.log.Warn("second shutdown signal — cancelling active runs", "active_runs", len(cancels))
	}
	for _, cancel := range cancels {
		cancel(errAgentStopped)
	}
}

// registerOrResume loads the persisted identity, or registers and saves a fresh
// one (R2.4).
func (a *Agent) registerOrResume(ctx context.Context) error {
	id, err := loadIdentity(a.cfg.IdentityFile)
	if err != nil {
		return err
	}
	if id != nil {
		a.id = *id
		a.log.Info("resumed runner identity", "id", id.ID)
		return nil
	}
	return a.register(ctx)
}

func (a *Agent) register(ctx context.Context) error {
	// Detect the toolchain capability tokens + display detail at registration
	// (RX.7). Best-effort: a runner without ansible still registers its configured
	// run-types. With no configured run-types the agent claims the shell types
	// and whichever local toolchains its host has (D1: 1B, detectRunTypes).
	a.log.Info("detecting host capabilities (toolchain probes)")
	caps, tc := detectCapabilities(ctx, a.cfg)
	a.log.Info("registering with server", "server", a.cfg.ServerURL, "capabilities", caps)
	// The exchange and the saving of its answer are one step that a stop signal
	// must not split (2.3.2). The registration token is good for ONE use: once
	// the server has answered, the token is spent and the runner exists, and an
	// agent that was stopped before it wrote its identity file can only ask
	// again with a spent token (401 token_used) and exit, for ever, until
	// somebody mints another. Seen on a host where the unit was restarted a
	// second after the installer started it. Bounded, so an unreachable server
	// cannot hold a stopping agent.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), registerTimeout)
	defer cancel()
	id, err := a.client.Register(rctx, a.cfg, caps, tc) //nolint:contextcheck // deliberate: see above
	if err != nil {
		return err
	}
	if err := saveIdentity(a.cfg.IdentityFile, id); err != nil {
		return err
	}
	a.id = id
	a.configDigest = a.declaredDigest(caps)
	a.log.Info("registered with server", "id", id.ID)
	return nil
}

// declaredDigest computes the canonical digest over exactly the declared set
// Register/Redeclare send (declareBody) — same inputs, same shared
// canonicalization (runnerproto.ConfigDigest), so server and agent agree by
// construction.
func (a *Agent) declaredDigest(caps []string) string {
	return runnerproto.ConfigDigest(a.cfg.Name, a.cfg.OS, caps,
		a.cfg.MaxConcurrent, a.cfg.Inventory, agentVersion, runnerproto.ProtocolVersion)
}

// reregister discards the stored identity and registers afresh — the shared
// recovery for a poll that proves the current identity is dead (404 reaped, or
// 401 token-rejected). A failed re-register is logged, not fatal: the next poll
// retries, so the runner self-heals the moment a valid registration token is in
// place (e.g. after the operator mints a new one), rather than needing manual
// identity-file surgery.
func (a *Agent) reregister(ctx context.Context, reason string) {
	a.log.Warn(reason, "old_id", a.id.ID)
	_ = discardIdentity(a.cfg.IdentityFile)
	if rerr := a.register(ctx); rerr != nil {
		a.log.Error("re-register failed", "error", rerr)
		return
	}
	// A new id is a new runner as far as the server is concerned, and it knows
	// nothing of this file until told.
	go a.reportKnownHosts(ctx)
}

// pollOnce performs one poll and dispatches its assignment + control ops. It
// never blocks on execution: a claimed run starts in a goroutine (bounded by
// the effective maxConcurrent) and the loop returns immediately so the next
// poll fires on cadence (R2.2). It echoes the applied managed-settings version
// as the poll ack (Phase 4) and applies any settings the response carries.
func (a *Agent) pollOnce(ctx context.Context) {
	// An agent with no free slot asks for nothing (2.3.2). The server's claim
	// does not know how many runs an agent holds, so a full agent that polled
	// as usual was handed the next queued run, could not start it, and left it
	// shown as running with nobody to end it.
	pr, err := a.client.Poll(ctx, a.id, a.configDigest, a.settings.applied(),
		pollState{started: !a.answered, noClaim: a.stopping || a.full()})
	if err == nil || errors.Is(err, errNoWork) {
		a.answered = true
	}
	switch {
	case errors.Is(err, errNoWork):
		// ET-D — a 204 is a COMPLETE answer, not a missing one: the server sends
		// it only when there is no assignment, no control, no settings AND no
		// watches (respondControlOr204), so it retracts the watch set exactly as
		// an empty list in a 200 body would. Clearing here is what makes
		// retraction-to-empty reachable at all — it is precisely the case that
		// turns the response into a 204, and the body-bearing path below never
		// runs for one, so without this an idle agent watches a deleted job's
		// paths forever.
		if a.watcher != nil {
			a.watcher.setSpecs(nil)
		}
		return
	case errors.Is(err, errReaped):
		// Server reaped/deregistered us (D4): discard identity and re-register.
		a.reregister(ctx, "server returned 404 on poll — re-registering")
		return
	case errors.Is(err, errIdentityRejected):
		// Server rejected our token (401): the runner was deleted or the token
		// store was reset. Same recovery as a 404 — without this the agent polls
		// a dead token forever and never reappears in the UI (a real incident).
		a.reregister(ctx, "server returned 401 on poll (token rejected) — discarding identity and re-registering")
		return
	case errors.Is(err, errProtocolTooOld):
		// Server refused our wire protocol (426). Deliberately NOT a re-register:
		// registering again declares the same protocol and is refused identically,
		// and discarding a valid identity would turn a fixable version gap into a
		// lost runner. Keep the identity, keep polling, and say so every time —
		// this line is the operator's signal, and it must not be a one-shot at
		// startup, because the agent that needs it has usually been running for
		// weeks before the server was upgraded underneath it.
		a.log.Error("poll refused: this agent is older than the server allows — upgrade the cronomicon-runner binary and restart; the runner keeps its identity and needs no deregistration",
			"error", err)
		return
	case err != nil:
		if ctx.Err() != nil && !a.stopping {
			return // the stop signal cancelled a poll the server was holding
		}
		a.log.Error("poll failed", "error", err)
		return
	}

	// Apply server-managed settings before dispatching, so a raised limit / new
	// checkout policy takes effect on this very poll's assignment.
	if a.settings.apply(pr.Settings) {
		a.log.Info("applied server-managed settings", "version", pr.Settings.Version)
	}

	// ET-D — refresh the watch set from EVERY poll that returns a body. The
	// server sends the complete list each time, so this is also how a removed
	// watch is retracted; an empty list means "watch nothing".
	if a.watcher != nil {
		a.watcher.setSpecs(pr.Watches)
	}

	a.handleControl(ctx, pr.Control)

	if pr.Assignment != nil {
		a.dispatch(ctx, pr.Assignment)
	}
}

// handleControl applies kill/drain/re-register control ops (R2.2, v4).
// Unknown ops are silently ignored (forward compat — an older agent talking to
// a newer server must not crash on a new op).
func (a *Agent) handleControl(ctx context.Context, control []runnerproto.PollControl) {
	for _, c := range control {
		switch c.Op {
		case "kill":
			if c.TraceID != nil {
				a.kill(*c.TraceID)
			}
		case "drain":
			a.startDrain("drain control received")
		case "re-register":
			a.redeclare(ctx)
		case "keyscan":
			// Scan + upload runs in its own goroutine so it never blocks the poll
			// loop (dials can be slow / time out).
			hosts := c.Hosts
			go a.handleKeyscan(ctx, hosts)
		case "untrust-hosts":
			a.handleUntrustHosts(c.Entries)
		case "trust-hosts":
			a.handleTrustHosts(c.Entries)
		case "known-hosts-report":
			// The server sends this AFTER any untrust/trust ops in the same
			// poll, and those two are applied synchronously above, so the file
			// is read as they left it. Only the upload leaves the poll loop.
			go a.reportKnownHosts(ctx)
		}
	}
}

// redeclare re-declares the agent's CURRENT local config in place (protocol v4
// resync — the operator's Resync button). It re-detects toolchain capabilities
// and POSTs the declared set with the EXISTING runner API key; the identity
// (id + key) is unchanged and nothing drains — active runs keep running.
func (a *Agent) redeclare(ctx context.Context) {
	caps, tc := detectCapabilities(ctx, a.cfg)
	a.configDigest = a.declaredDigest(caps) // keep poll digest in step with what we declare
	err := a.client.Redeclare(ctx, a.id, a.cfg, caps, tc)
	switch {
	case errors.Is(err, errReaped):
		// Row gone between the op and this call (reaped/deregistered). The next
		// poll's 404 takes the discard-identity + fresh-register path, which is
		// the correct recovery — a reaped runner needs a registration token.
		a.log.Warn("redeclare: runner no longer exists on server — next poll will re-register fresh")
	case err != nil:
		a.log.Error("redeclare failed", "error", err)
	default:
		a.log.Info("re-declared configuration with server (resync)",
			"id", a.id.ID, "capabilities", caps)
	}
}

// kill cancels the execution context of the matching run, tearing down its SSH
// sessions / killing its local process.
func (a *Agent) kill(traceID string) {
	a.mu.Lock()
	cancel, ok := a.active[traceID]
	a.mu.Unlock()
	if ok {
		a.log.Info("kill control received", "trace_id", traceID)
		cancel(nil)
	}
}

// startDrain stops claiming new work; active runs are allowed to finish (R2.2).
// why is what asked for it (the server's drain op, or a stop signal). Returns
// the number of runs in flight.
func (a *Agent) startDrain(why string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.draining {
		a.draining = true
		a.log.Info(why + " — finishing active runs, claiming no new work")
	}
	return len(a.active)
}

// isDrainingDone reports whether the agent is draining and has no active runs.
func (a *Agent) isDrainingDone() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.draining && len(a.active) == 0
}

// dispatch starts executing a claimed run, unless we're draining or out of
// concurrency slots. It runs in a goroutine so the poll loop never blocks.
func (a *Agent) dispatch(ctx context.Context, asn *runnerproto.PollAssignment) {
	// Honor the server-managed capability mask agent-side too (Phase 4). The
	// server already refuses to assign a masked run-type; this catches a stale
	// assignment that raced a mask change.
	if a.settings.masks(asn.RunType) {
		a.refuse(ctx, asn, "its settings no longer let it run "+asn.RunType+" jobs")
		return
	}

	// Acquire a concurrency slot and record the run under one lock: len(active)
	// is the live count, bounded by the effective (possibly server-managed)
	// maxConcurrent. If full or draining, the server claimed a run we cannot
	// start: refuse says so to the server, which has no other way to learn it.
	//
	// The run's context descends from runParent, not from the poll's: a stop
	// signal cancels the poll loop's context and must not reach a run (Run).
	parent := a.runParent //nolint:contextcheck // deliberate: a run outlives the stop signal (Run)
	if parent == nil {
		parent = ctx
	}
	runCtx, cancel := context.WithCancelCause(parent)
	a.mu.Lock()
	if a.draining {
		a.mu.Unlock()
		cancel(nil)
		a.refuse(ctx, asn, "it is stopping and takes no new work")
		return
	}
	if len(a.active) >= a.settings.maxConcurrent(a.cfg.MaxConcurrent) {
		a.mu.Unlock()
		cancel(nil)
		a.refuse(ctx, asn, "it is already running as many jobs as its limit allows")
		return
	}
	a.active[asn.TraceID] = cancel
	a.mu.Unlock()

	a.wg.Go(func() {
		defer cancel(nil)
		defer func() {
			a.mu.Lock()
			wasFull := len(a.active) >= a.settings.maxConcurrent(a.cfg.MaxConcurrent)
			delete(a.active, asn.TraceID)
			last := len(a.active) == 0
			a.mu.Unlock()
			if last {
				select {
				case a.idle <- struct{}{}:
				default:
				}
			}
			if wasFull {
				select {
				case a.freed <- struct{}{}:
				default:
				}
			}
		}()
		a.executeRun(runCtx, asn.TraceID, asn.LiveLog)
	})
}

// liveLogFlushInterval is how often a LiveLog run's pending buffer is flushed
// to the server mid-run (v12 live tailing). Whole lines only — writeLine always
// appends newline-terminated lines, so a flush can never split a line (and thus
// never splits a secret across the server's per-line redaction).
const liveLogFlushInterval = 2 * time.Second

// executeRun fetches the manifest, executes it, and streams the resulting log
// (with resume) to the server. liveLog (v12) marks a server that accepts
// mid-run partial chunks: the buffer is then flushed on a short ticker DURING
// execution so operators can tail the log live, with the sealed terminal flush
// unchanged at the end.
func (a *Agent) executeRun(ctx context.Context, traceID string, liveLog bool) {
	m, err := a.client.Manifest(ctx, a.id, traceID)
	if err != nil {
		a.log.Error("fetch manifest", "trace_id", traceID, "error", err)
		// Without a manifest we can't execute; stream a single failure line +
		// envelope so the run terminates instead of hanging.
		buf := &logBuffer{}
		buf.writeLine("cronomicon: manifest fetch failed: " + err.Error())
		buf.seal(makeEnvelope(1, time.Now(), time.Now(), ""))
		if serr := streamLogs(ctx, a.client, a.id, traceID, buf, a.cfg.LogRetryBudget, false); serr != nil {
			a.log.Error("stream logs (manifest failure)", "trace_id", traceID, "error", serr)
		}
		return
	}

	buf := &logBuffer{}

	// v12 live tailing: flush pending lines every few seconds while the body
	// executes. Best-effort with budget 1 — a failed tick (network blip, the
	// server's fail-closed 503 during a Vault outage) just leaves the bytes
	// pending for the next tick or the terminal flush; the resume-offset
	// machinery makes re-sends lossless either way. The ticker goroutine is the
	// ONLY concurrent caller of streamLogs, and it is stopped and joined before
	// the terminal flush below, so two flushes never interleave.
	var flushWG sync.WaitGroup
	var stopFlush context.CancelFunc
	if liveLog {
		var flushCtx context.Context
		flushCtx, stopFlush = context.WithCancel(ctx)
		flushWG.Go(func() {
			t := time.NewTicker(liveLogFlushInterval)
			defer t.Stop()
			for {
				select {
				case <-flushCtx.Done():
					return
				case <-t.C:
					if err := streamLogs(flushCtx, a.client, a.id, traceID, buf, 1, true); err != nil {
						a.log.Debug("live log flush skipped", "trace_id", traceID, "error", err)
					}
				}
			}
		})
	}

	exitCode := a.exec.run(ctx, m, buf)
	a.log.Info("run finished", "trace_id", traceID, "exit_code", exitCode)

	if stopFlush != nil {
		stopFlush()
		flushWG.Wait()
	}

	// The terminal flush outlives the run's own cancellation (2.3.2). A run that
	// was cancelled still has a log and an exit code, and the server has no
	// other way to learn either: flushing on ctx sent nothing at all for a run
	// the agent had just ended. Bounded, so a server that never answers cannot
	// hold a stopping agent. For a run the server killed, it has already
	// written the terminal state and answers 409; the budget ends that quickly.
	flush, stop := context.WithTimeout(context.WithoutCancel(ctx), terminalFlushTimeout)
	defer stop()
	switch err := streamLogs(flush, a.client, a.id, traceID, buf, a.cfg.LogRetryBudget, false); {
	case errors.Is(err, errRunClosed):
		a.log.Info("the server had already closed this run; the rest of its log was not accepted", "trace_id", traceID)
	case err != nil:
		// Budget exhausted: the server will mark the run log_stream_lost (R2.3).
		a.log.Error("log stream lost", "trace_id", traceID, "error", err)
	}
}

// errRunClosed: see streamLogs.
var errRunClosed = errors.New("the server has already closed this run")

// terminalFlushTimeout bounds the upload of a finished run's log tail and
// envelope. Generous beside the retry budget's own back-off (seconds).
const terminalFlushTimeout = 2 * time.Minute
