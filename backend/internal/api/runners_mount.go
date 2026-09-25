package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
)

// requireRunnerAgency wraps a state-changing OPERATOR runner route with the
// RF-2/RF-Q3 departmental gate (the RBAC-fixes plan): the caller must
// hold configureApp on an agency the runner belongs to. A runner with NO agency
// is a GENERAL-POOL runner — it executes every department's unscoped and system
// work, so writes on it are unrestricted-only (the RB-Q14 mirror, ratified as
// RF-Q3): one department's admin draining or reconfiguring the shared fleet is
// cross-department tampering. Runner-bearer routes (poll, redeclare, hostkey
// upload) are the agent itself and are NOT wrapped; nor are reads.
//
// Sits INSIDE requirePerm, so the coarse "holds configureApp at all" gate and
// its audit stay exactly as they were.
func (s *Server) requireRunnerAgency(pathVar string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
			"runner_agencies", "runner_id", r.PathValue(pathVar), "runner") {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireFleetWide gates an act that has no single owning entity and affects the
// whole fleet — today, runner registration tokens (RF-2/RF-Q3). It requires
// configureApp on an UNRESTRICTED grant, the same bar RB-Q14 sets for changing
// shared infrastructure, because "belongs to no department" and "belongs to all
// of them" are the same row from opposite sides.
func (s *Server) requireFleetWide(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if !id.CanAgency(auth.PermConfigureApp, "") {
			s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
				"runner registration is fleet-wide — a new runner joins the general pool and "+
					"serves every department, so only an unrestricted operator may mint or revoke its tokens")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireHostKeyRunnerAgency is requireRunnerAgency for the host-key resolve
// route, whose path names the pending KEY — the owning runner is looked up
// first. A missing key falls through to the handler's own 404 so this wrapper
// adds no existence oracle.
func (s *Server) requireHostKeyRunnerAgency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		var runnerID string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT runner_id FROM pending_host_keys WHERE id = ?`, r.PathValue("keyId")).Scan(&runnerID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if err == nil {
			// Approving a host key changes what the runner will trust — a write on
			// the runner in every way that matters (RF-Q3).
			if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
				"runner_agencies", "runner_id", runnerID, "runner") {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// mountRunners wires the B4 runner subsystem routes (§6.1–6.6, T6, T7/S7).
//
// Route summary:
//
//	GET    /api/v1/runners                       operator: list runners
//	POST   /api/v1/runners/register              runner bearer (reg token): register
//	GET    /api/v1/runners/registration-tokens        operator: list reg tokens (Phase 7)
//	POST   /api/v1/runners/registration-tokens        operator+CSRF: mint a single-use token
//	DELETE /api/v1/runners/registration-tokens/{id}   operator+CSRF: revoke an unused token
//	GET    /api/v1/runners/{id}/poll             runner bearer: long-poll for work (A6.2)
//	POST   /api/v1/runners/{id}/drain            operator+CSRF: drain (A6.4)
//	POST   /api/v1/runners/{id}/resync           operator+CSRF: request re-declare (Phase 4)
//	POST   /api/v1/runners/{id}/redeclare        runner bearer: id-preserving re-declare (v4)
//	DELETE /api/v1/runners/{id}                  operator+CSRF: deregister
//	GET    /api/v1/runs/{traceId}/manifest        runner bearer: execution manifest (R1.2/D2)
//	POST   /api/v1/runs/{traceId}/log            runner bearer: chunked log ingest (T6)
//	GET    /api/v1/runs/{traceId}/log            operator: read redacted log (S7)
func (s *Server) mountRunners(mux *http.ServeMux) {
	// Wire notification dispatch (C.1): the runner fires it when a run reaches a
	// terminal status (failure/success per the operator's alert rules).
	svc := runner.New(s.db, s.cfg, s.log).
		WithNotifier(notify.New(s.db, s.cfg, s.log)).
		// LU-9: this is the instance that serves HandleGetLog, so it is the one
		// that needs to record its scope denials.
		WithAuthAudit(s.auth).
		// SL-3: the reader falls back to the archive tier for a reaped local
		// log. Per-read getter, so a settings save re-points it without restart.
		WithLogArchive(s.LogArchive)
	// DB-backed log dir (E.5), resolved once here and then pushed to every
	// consumer whenever the operator changes it (LU-5) — no restart required.
	// Both services are retained on the Server for exactly that reason: before
	// LU-5 they were constructed here and dropped, so nothing could reach them.
	logDir := settings.ResolveLogDir(context.Background(), s.db)
	svc.SetLogDir(logDir)
	s.runnerSvc = svc
	s.logDir.Store(&logDir)

	// In-app SSH executor (execution-update.md EX.4/EX.6): opt-in worker pool
	// that claims executor='ssh' runs and executes them over SSH. Disabled by
	// default — Start() no-ops unless CRONOMICON_SSH_EXECUTOR_ENABLED is set (it
	// still runs the startup orphan sweep). Bound to the process context (PP-M6)
	// so SIGTERM unwinds it, and registered with the shutdown WaitGroup (PP-L15)
	// so in-flight runs finalize before the pool closes.
	sshSvc := sshexec.New(s.db, s.cfg, s.log, gitlab.DefaultCloneDir(), logDir).
		WithShutdownWG(s.shutdownWG)
	s.sshExec = sshSvc
	sshSvc.Start(s.runCtx)

	// ── Operator: list runners ─────────────────────────────────────────────
	mux.Handle("GET /api/v1/runners",
		s.auth.RequireSession(http.HandlerFunc(svc.HandleListRunners)))

	// Operator: set a runner's tags (operator-authored, SQLite-only; migration
	// 580). ConfigureApp + CSRF, matching the runner↔agency membership write.
	mux.Handle("PUT /api/v1/runner-tags/{runnerId}",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("runnerId", http.HandlerFunc(s.updateRunnerTags))))

	// ── Runner: register (bearer = shared registration token) ─────────────
	// Note: RequireRunner accepts any valid runner_tokens entry; the
	// registration endpoint additionally validates registration-specific tokens
	// (registration_tokens table + bootstrap token). The handler does its own
	// auth so we wrap with a no-op session-free handler.
	mux.HandleFunc("POST /api/v1/runners/register", svc.HandleRegisterRunner)

	// ── Operator: registration token management (§6.6, single-use since
	// Phase 7 — mint per install, revoke unused; the shared-token GET/rotate
	// endpoints are gone) ──────────────────────────────────────────────────
	// Minting and revoking are FLEET-WIDE acts, not per-runner ones, so they take
	// the unrestricted bar rather than a per-entity agency check (RF-2/RF-Q3):
	// a registration token enrolls a runner that registers with NO agency — the
	// general pool — which then claims every department's unscoped and system work,
	// including injected secret material. Letting one department's admin mint that
	// is the same cross-department reach RF-Q3 closed on the drain/settings routes,
	// arrived at from the other side. Revoking is fleet-wide denial of enrollment.
	mux.Handle("GET /api/v1/runners/registration-tokens",
		s.auth.RequireSession(http.HandlerFunc(svc.HandleListRegistrationTokens)))
	mux.Handle("POST /api/v1/runners/registration-tokens",
		s.requirePerm("configureApp", permConfigureApp)(s.requireFleetWide(http.HandlerFunc(svc.HandleMintRegistrationToken))))
	mux.Handle("DELETE /api/v1/runners/registration-tokens/{id}",
		s.requirePerm("configureApp", permConfigureApp)(s.requireFleetWide(http.HandlerFunc(svc.HandleRevokeRegistrationToken))))

	// ── Runner: long-poll for work (A6.2, doubles as heartbeat) ───────────
	// Path: GET /api/v1/runners/{id}/poll
	mux.Handle("GET /api/v1/runners/{id}/poll",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandlePoll)))

	// ── Drain (ConfigureApp + owning agency, RF-2 — see requireRunnerAgency) ──
	mux.Handle("POST /api/v1/runners/{id}/drain",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleDrain))))

	// ── Resync (ConfigureApp — fleet op; runner-install-update.md Phase 4) ─
	// Sets resync_requested; the next poll delivers PollControl{Op:"re-register"}.
	// (The protocol >= 4 gate and its 409 went with the floor in v1.5.40: every
	// registered agent speaks the current protocol.)
	mux.Handle("POST /api/v1/runners/{id}/resync",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleResync))))

	// ── Managed settings (ConfigureApp — fleet op; plan 2 Phase 4) ────────
	// Replaces the runner's server-managed override set and bumps the version;
	// the next poll delivers it to a settings-capable agent, applied in-memory.
	mux.Handle("PATCH /api/v1/runners/{id}/settings",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleUpdateRunnerSettings))))

	// ── Secret-injection trust flag (ConfigureApp; vault-integration.md P1.4) ──
	// Operator grant permitting this runner to claim binding-bearing runs and
	// receive injected secret material in the manifest. Operator-set, never
	// self-declared.
	mux.Handle("PUT /api/v1/runners/{id}/secret-injection",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleSetSecretInjection))))

	// ── Host-key scan & approve (plan 2 Phase 5, D4: 4A) ──────────────────
	// Operator: queue a scan (delivered as a v5 keyscan op), list pending scanned
	// keys, and approve/reject one (approval stages a trust-hosts op).
	mux.Handle("POST /api/v1/runners/{id}/keyscan",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleKeyscan))))
	mux.Handle("GET /api/v1/runners/host-keys/pending",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(svc.HandleListPendingHostKeys)))
	mux.Handle("POST /api/v1/runners/host-keys/{keyId}/resolve",
		s.requirePerm("configureApp", permConfigureApp)(s.requireHostKeyRunnerAgency(http.HandlerFunc(svc.HandleResolveHostKey))))
	// Runner: upload the keys it scanned (runner-key auth, ownership-guarded).
	// ET-D: the agent reports what it OBSERVED; the server decides what that
	// means and does the firing. Mirrors the host-key upload (v5) — same
	// bearer auth, same ownership guard, same "agent never acts, only
	// reports" shape.
	mux.Handle("POST /api/v1/runners/{id}/file-sightings",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleFileSightings)))
	mux.Handle("POST /api/v1/runners/{id}/hostkeys",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleUploadHostKeys)))

	// ── Runner: id-preserving re-declare (protocol v4) ────────────────────
	// Authenticated by the runner's own crn_run_* key (never a registration
	// token — D6); ownership guard inside the handler, like poll.
	mux.Handle("POST /api/v1/runners/{id}/redeclare",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleRedeclare)))

	// ── Accept a placement suggestion (DR-7 (c); gate corrected DRF-9) ────
	// ConfigureApp at the mount; the source-runner gate AND the target-agency
	// check are inside the handler. The first draft skipped the source gate on
	// the theory that it "would make delegation impossible" for an unbound
	// runner. That was backwards: RF-Q3 says the general pool is SHARED
	// infrastructure a restricted admin may not touch, and an unbound runner IS
	// the general pool — so skipping the gate made this endpoint a bypass of the
	// rule PUT /runner-agencies already enforces. Same gate, same answer.
	mux.Handle("POST /api/v1/runners/{id}/placement",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleAcceptRunnerPlacement(w, r, svc)
		})))

	// ── Dismiss a placement suggestion (DRF-3) — same gate as accept ──────
	mux.Handle("POST /api/v1/runners/{id}/placement/dismiss",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleDismissRunnerPlacement(w, r, svc)
		})))

	// ── Deregister (ConfigureApp + owning agency, RF-2) ───────────────────
	mux.Handle("DELETE /api/v1/runners/{id}",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleDeregisterRunner))))

	// ── Test Runner Connection (ConfigureApp + owning agency, RF-2) ───────
	mux.Handle("POST /api/v1/runners/{id}/test",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(svc.HandleTestRunner))))

	// ── Runner: execution manifest (R1.2/D2) ──────────────────────────────
	// Fetched after claim. Authorized by run ownership inside the handler (R1.4).
	mux.Handle("GET /api/v1/runs/{traceId}/manifest",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleGetManifest)))

	// ── Log ingest (runner) + Log read (operator) ─────────────────────────
	// POST = runner bearer (T6 chunked ingest); GET = operator session (S7 redacted read).
	mux.Handle("POST /api/v1/runs/{traceId}/log",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)))
	mux.Handle("GET /api/v1/runs/{traceId}/log",
		s.auth.RequireSession(http.HandlerFunc(svc.HandleGetLog)))
}

// handleAcceptRunnerPlacement accepts a placement suggestion for an unbound
// runner (DR-7 (c)).
//
// DR-Q2 in one sentence: this endpoint is the human confirmation. The server
// never re-binds a re-registered runner on its own, because the name a
// suggestion is looked up by is self-declared by the agent — so an automatic
// heal would let any agent inherit another runner's agency by claiming its name.
//
// Authorisation is EXACTLY what PUT /runner-agencies requires, so accepting can
// never reach further than placing by hand (DRF-9):
//
//  1. the source gate — requireEntityAgency on the runner. For the runner this
//     endpoint exists for, one with NO agency, that resolves to "shared by every
//     department; unrestricted only" (RF-Q3). A departmental operator therefore
//     cannot accept, exactly as they cannot place that runner manually;
//  2. the target check — CanAgency(configureApp) on EVERY agency the snapshot
//     names, which for an unrestricted caller is always true. Kept anyway, so
//     the invariant survives if the source gate's rule is ever relaxed.
func (s *Server) handleAcceptRunnerPlacement(w http.ResponseWriter, r *http.Request, svc *runner.Service) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
		"runner_agencies", "runner_id", r.PathValue("id"), "runner") {
		return
	}
	var body struct {
		HistoryID int64 `json:"historyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.HistoryID == 0 {
		httpx.Fail(w, http.StatusBadRequest, "invalid_body", "historyId is required")
		return
	}

	permits := func(agencyID string) bool {
		return id.Unrestricted() || id.CanAgency(auth.PermConfigureApp, agencyID)
	}
	err := svc.ApplyPlacement(r.Context(), r.PathValue("id"), body.HistoryID, id.Email, permits)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, runner.ErrPlacementTags):
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_tags",
			"restoring these tags would break the tag rules; trim the runner's tags and retry")
	case errors.Is(err, runner.ErrPlacementForbidden):
		httpx.Fail(w, http.StatusForbidden, "forbidden",
			"you do not have configureApp on every agency this placement would restore")
	case errors.Is(err, runner.ErrPlacementGone):
		httpx.Fail(w, http.StatusConflict, "placement_stale",
			"this suggestion no longer applies — the runner may have been placed, removed or renamed since")
	default:
		httpx.Fail500(w, s.log, "db_error", err)
	}
}

// handleDismissRunnerPlacement withdraws a suggestion (DRF-3). Gated exactly
// like accept: dismissing is a decision about placement, and the runner it
// concerns is in no agency, so RF-Q3 applies.
func (s *Server) handleDismissRunnerPlacement(w http.ResponseWriter, r *http.Request, svc *runner.Service) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
		"runner_agencies", "runner_id", r.PathValue("id"), "runner") {
		return
	}
	var body struct {
		HistoryID int64 `json:"historyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.HistoryID == 0 {
		httpx.Fail(w, http.StatusBadRequest, "invalid_body", "historyId is required")
		return
	}
	err := svc.DismissPlacement(r.Context(), r.PathValue("id"), body.HistoryID, id.Email)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, runner.ErrPlacementGone):
		httpx.Fail(w, http.StatusConflict, "placement_stale",
			"this suggestion no longer exists or was already dismissed")
	default:
		httpx.Fail500(w, s.log, "db_error", err)
	}
}
