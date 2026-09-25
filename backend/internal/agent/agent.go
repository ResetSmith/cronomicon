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
	active   map[string]context.CancelFunc
	draining bool
	wg       sync.WaitGroup

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
		active:   map[string]context.CancelFunc{},
		settings: store,
	}, nil
}

// Run drives the agent until ctx is cancelled. It registers (or resumes its
// identity), then loops polling at the configured cadence. It always keeps
// polling while runs execute (R2.2) so it stays heartbeat-fresh and receives
// kill/drain. On a 404 poll it re-registers (D4).
func (a *Agent) Run(ctx context.Context) error {
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
	sb := false
	if !a.cfg.NoSandbox {
		a.log.Info("probing tier-2 sandbox availability (systemd-run)")
		sb = probeSandbox(ctx)
	} else {
		a.log.Info("sandbox probe skipped (NoSandbox set)")
	}
	a.cfg.SandboxAvailable = sb
	a.exec.cfg.SandboxAvailable = sb
	if !sb {
		if a.cfg.AllowCheckout {
			a.log.Warn("SANDBOX UNAVAILABLE — -allow-checkout is set but no usable systemd-run scope could be created; checkout runs will execute UNSANDBOXED (only their timeout applies). Each run is reported as unsandboxed; a job may require [sandboxed] to avoid landing here.")
		} else {
			a.log.Info("tier-2 sandbox unavailable (no usable systemd-run) — local-toolchain runs execute unsandboxed")
		}
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
		caps, _, err := detectCapabilities(ctx, a.cfg)
		if err != nil {
			return err
		}
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

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	for {
		a.pollOnce(ctx)

		if a.isDrainingDone() {
			a.log.Info("drain complete — no active runs, exiting")
			return nil
		}

		select {
		case <-ctx.Done():
			a.log.Info("shutdown signal — waiting for active runs to finish")
			a.wg.Wait()
			return ctx.Err()
		case <-ticker.C:
		}
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
	// run-types. With no configured run-types the whole set is probed from the
	// host (D1: 1B), and a probe that finds nothing aborts the registration.
	a.log.Info("detecting host capabilities (toolchain probes)")
	caps, tc, err := detectCapabilities(ctx, a.cfg)
	if err != nil {
		return err
	}
	a.log.Info("registering with server", "server", a.cfg.ServerURL, "capabilities", caps)
	id, err := a.client.Register(ctx, a.cfg, caps, tc)
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
	}
}

// pollOnce performs one poll and dispatches its assignment + control ops. It
// never blocks on execution: a claimed run starts in a goroutine (bounded by
// the effective maxConcurrent) and the loop returns immediately so the next
// poll fires on cadence (R2.2). It echoes the applied managed-settings version
// as the poll ack (Phase 4) and applies any settings the response carries.
func (a *Agent) pollOnce(ctx context.Context) {
	pr, err := a.client.Poll(ctx, a.id, a.configDigest, a.settings.applied())
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
			a.startDrain()
		case "re-register":
			a.redeclare(ctx)
		case "keyscan":
			// Scan + upload runs in its own goroutine so it never blocks the poll
			// loop (dials can be slow / time out).
			hosts := c.Hosts
			go a.handleKeyscan(ctx, hosts)
		case "trust-hosts":
			a.handleTrustHosts(c.Entries)
		}
	}
}

// redeclare re-declares the agent's CURRENT local config in place (protocol v4
// resync — the operator's Resync button). It re-detects toolchain capabilities
// and POSTs the declared set with the EXISTING runner API key; the identity
// (id + key) is unchanged and nothing drains — active runs keep running.
func (a *Agent) redeclare(ctx context.Context) {
	caps, tc, err := detectCapabilities(ctx, a.cfg)
	if err != nil {
		// Auto-detect found nothing (toolchains vanished since startup?). Keep
		// the current declaration rather than declaring an unclaimable runner.
		a.log.Error("redeclare skipped", "error", err)
		return
	}
	a.configDigest = a.declaredDigest(caps) // keep poll digest in step with what we declare
	err = a.client.Redeclare(ctx, a.id, a.cfg, caps, tc)
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
		cancel()
	}
}

// startDrain stops claiming new work; active runs are allowed to finish (R2.2).
func (a *Agent) startDrain() {
	a.mu.Lock()
	if !a.draining {
		a.draining = true
		a.log.Info("drain control received — finishing active runs, claiming no new work")
	}
	a.mu.Unlock()
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
		a.log.Warn("run-type masked by server-managed settings — refusing assignment", "trace_id", asn.TraceID, "type", asn.RunType)
		return
	}

	// Acquire a concurrency slot and record the run under one lock: len(active)
	// is the live count, bounded by the effective (possibly server-managed)
	// maxConcurrent. If full, the server claimed a run we can't start right now —
	// log and let the run's absence of logs trip the server-side reconcile.
	runCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.draining {
		a.mu.Unlock()
		cancel()
		a.log.Warn("draining — dropping claimed assignment (will not execute)", "trace_id", asn.TraceID)
		return
	}
	if len(a.active) >= a.settings.maxConcurrent(a.cfg.MaxConcurrent) {
		a.mu.Unlock()
		cancel()
		a.log.Warn("at max concurrency — cannot start claimed run now", "trace_id", asn.TraceID)
		return
	}
	a.active[asn.TraceID] = cancel
	a.mu.Unlock()

	a.wg.Go(func() {
		defer cancel()
		defer func() {
			a.mu.Lock()
			delete(a.active, asn.TraceID)
			a.mu.Unlock()
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

	if err := streamLogs(ctx, a.client, a.id, traceID, buf, a.cfg.LogRetryBudget, false); err != nil {
		// Budget exhausted: the server will mark the run log_stream_lost (R2.3).
		a.log.Error("log stream lost", "trace_id", traceID, "error", err)
	}
}
