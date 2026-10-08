package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
)

// requireHostKeyBatchRunnerAgency gates a batch read by the runner the batch is
// about. An unknown batch passes through to the handler's 404.
func (s *Server) requireHostKeyBatchRunnerAgency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		var runnerID string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT runner_id FROM host_key_ledger WHERE batch_id = ? LIMIT 1`, r.PathValue("batchId")).Scan(&runnerID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if err == nil && !s.requireRunnerOwner(w, r, id, runnerID) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleListPendingHostKeys lists the scanned keys awaiting review across the
// fleet, minus the runners the caller has no authority over. The rows name
// hosts — since SB, the dial addresses of whole scopes — so this read carries
// the same departmental gate as the per-runner ledger routes, applied per row:
// an agency's admin sees the keys of the runners their agency OWNS and nobody
// else's, and a Global-owned runner's only as a global administrator (LR-59).
func (s *Server) handleListPendingHostKeys(svc *runner.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		all, err := svc.PendingHostKeys(r.Context())
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		out := []runner.PendingHostKey{}
		allowed := map[string]bool{}
		for _, p := range all {
			may, seen := allowed[p.RunnerID]
			if !seen {
				owner, _, err := s.runnerOwner(r.Context(), p.RunnerID)
				if err != nil {
					httpx.Fail500(w, s.log, "db_error", err)
					return
				}
				may = id.CanAgency(auth.PermConfigureApp, owner)
				allowed[p.RunnerID] = may
			}
			if may {
				out = append(out, p)
			}
		}
		httpx.JSON(w, http.StatusOK, out)
	}
}

// runnerOwner reads the agency that owns a runner (runners.owner_agency, LR-58).
// found reports a LIVE runner.
//
// A deregistered runner still has a record — its host-key ledger, the keys a
// replacement carries over from it — and that record is its former owner's: the
// snapshot taken at deregistration kept the owner (runner_placement_history,
// written by the server in the deleting transaction; runner ids are never
// reused). An id with neither a row nor a snapshot reads as Global's, so a
// caller who is not a global administrator gets the same refusal for "not
// yours" and "does not exist".
func (s *Server) runnerOwner(ctx context.Context, runnerID string) (owner string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT owner_agency FROM runners WHERE id = ?`, runnerID).Scan(&owner)
	if err == nil {
		return owner, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT owner_agency FROM runner_placement_history
		 WHERE runner_id = ? ORDER BY id DESC LIMIT 1`, runnerID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return agencyid.Global, false, nil
	case err != nil:
		return "", false, err
	}
	// The agency may have been deleted since. Its record is then nobody's but a
	// global administrator's.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id = ?`, owner).Scan(&n); err != nil {
		return "", false, err
	}
	if n == 0 {
		return agencyid.Global, false, nil
	}
	return owner, false, nil
}

// requireRunnerOwner is THE runner gate (LR-59): configureApp on the agency
// that owns the runner. Global's are a global administrator's — a Global-owned
// agent, a legacy placement (an agent that still serves several agencies, or
// one that is not its owner's) and, from Phase A, the local runner.
//
// Until 2.3.0 the question was "does the caller administer ANY agency this
// runner is in", read from the serve list. That let each of several agencies
// drain, reconfigure and re-key a runner the others depended on. A runner has
// one owner now, and "who administers this" has one answer.
func (s *Server) requireRunnerOwner(w http.ResponseWriter, r *http.Request, id auth.Identity, runnerID string) bool {
	owner, _, err := s.runnerOwner(r.Context(), runnerID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	if id.CanAgency(auth.PermConfigureApp, owner) {
		return true
	}
	if owner == agencyid.Global {
		s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
			"this runner is Global's, so only a global administrator may manage it")
		return false
	}
	s.denyEntityAgency(w, r, id, auth.PermConfigureApp, owner,
		"you do not have "+auth.PermConfigureApp+" on the agency that owns this runner")
	return false
}

// requireRunnerAgency wraps a state-changing OPERATOR runner route with the
// owner gate (requireRunnerOwner). Runner-bearer routes (poll, redeclare,
// hostkey upload) are the agent itself and are NOT wrapped; nor is the list.
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
		if !s.requireRunnerOwner(w, r, id, r.PathValue(pathVar)) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// agentOnly refuses a route for the LOCAL runner (kind `server`), inside the
// owner gate, so only someone who may manage the runner learns which kind it
// is. The local runner is this server: there is no agent to drain, re-declare,
// reconfigure or test, it is turned on and off with its own setting
// (PUT /local-runner), secret injection is fixed on for it (LR-46), and its row
// is never deregistered (LR-39). Its host keys are still the server's own, kept
// with the SSH targets, until they move to the runner's ledger (Phase C of the
// local runner); until then the host-key routes refuse it too.
//
// 422 `local_runner`, with what to do instead.
func (s *Server) agentOnly(pathVar string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isAgent(w, r, r.PathValue(pathVar)) {
			next.ServeHTTP(w, r)
		}
	})
}

// isAgent is agentOnly for a handler that runs its owner gate itself: call it
// AFTER that gate, so someone who may not manage the runner gets the ordinary
// 403 and not this. It writes the refusal and returns false for the local
// runner.
func (s *Server) isAgent(w http.ResponseWriter, r *http.Request, runnerID string) bool {
	local, err := settings.IsLocalRunner(r.Context(), s.db, runnerID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	if local {
		httpx.Fail(w, http.StatusUnprocessableEntity, "local_runner",
			"this is the local runner — the server itself — so there is no agent here to act on. "+
				"Turn it on or off, and set how many runs it takes, under Settings → Local runner; "+
				"its host keys are managed with the SSH targets")
		return false
	}
	return true
}

// requireRunnerOwnerOrHostKeyGuest is the owner gate with LR-63's one, narrow
// exception, for the three routes that make up "review a host's key": queue a
// scan of a scope, list what the scan found, approve or reject it.
//
// The exception exists for a runner that serves agencies which do not own it:
// a legacy placement today (MA-9), the local runner from Phase A. Such a runner
// is a global administrator's, and without this a department would wait on one
// for every new host. So an administrator of an agency the runner SERVES may
// review that runner's keys for the hosts of their own agency's scopes — and
// nothing else: not typed hosts, not a pasted key, not another agency's scope,
// not removing a key, not the ledger.
//
// An agency's own agent needs no exception: its administrators own it.
//
// The limit travels to the handler as the scope ids the guest may act on
// (runner.WithHostKeyScopes). A handler that did not read it would treat a
// guest as an owner, which is why this wrapper is applied to exactly the routes
// whose handlers do, and every other runner route keeps requireRunnerAgency.
func (s *Server) requireRunnerOwnerOrHostKeyGuest(pathVar string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		runnerID := r.PathValue(pathVar)
		owner, found, err := s.runnerOwner(r.Context(), runnerID)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if id.CanAgency(auth.PermConfigureApp, owner) {
			next.ServeHTTP(w, r)
			return
		}
		if found {
			scopes, err := s.hostKeyGuestScopes(r.Context(), id, runnerID, owner)
			if err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if scopes != nil {
				next.ServeHTTP(w, r.WithContext(runner.WithHostKeyScopes(r.Context(), scopes)))
				return
			}
		}
		// Refused, in the owner gate's words. Written here from the owner read
		// above rather than by asking the gate again: a second read could
		// disagree with the first, and then nothing would be written at all.
		if owner == agencyid.Global {
			s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
				"this runner is Global's, so only a global administrator may manage it")
			return
		}
		s.denyEntityAgency(w, r, id, auth.PermConfigureApp, owner,
			"you do not have "+auth.PermConfigureApp+" on the agency that owns this runner")
	})
}

// hostKeyGuestScopes returns the scopes whose hosts' keys the caller may review
// on a runner they do not own (LR-63): the scopes of every agency that the
// runner serves, that is not the runner's owner, and that the caller holds
// configureApp on. nil means the exception does not apply at all; an empty
// non-nil list means it applies and the agency has no scope yet.
func (s *Server) hostKeyGuestScopes(ctx context.Context, id auth.Identity, runnerID, owner string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id`, runnerID)
	if err != nil {
		return nil, err
	}
	var held []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		if a != owner && id.CanAgency(auth.PermConfigureApp, a) {
			held = append(held, a)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(held) == 0 {
		return nil, nil
	}
	scopes := []string{}
	for _, a := range held {
		srows, err := s.db.QueryContext(ctx, `SELECT scope_id FROM scope_agencies WHERE agency_id = ?`, a)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var sid string
			if err := srows.Scan(&sid); err != nil {
				srows.Close()
				return nil, err
			}
			scopes = append(scopes, sid)
		}
		if err := srows.Err(); err != nil {
			srows.Close()
			return nil, err
		}
		srows.Close()
	}
	return scopes, nil
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
			if !s.requireRunnerOwner(w, r, id, runnerID) {
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
	// A token names the agency that will OWN the agent it enrols (LR-61), so
	// minting one is granting that agency a runner, and the gate is configureApp
	// on that agency: an agency's administrators enrol their own agents, and a
	// global administrator enrols one for any agency or for Global (MA-3).
	//
	// Until 2.3.0 these were a global administrator's alone, because every new
	// runner registered into the general pool and then claimed every department's
	// unscoped work. An agent now serves exactly the agency its token names and
	// nothing else, so there is nothing fleet-wide about it. The list and the
	// revoke follow the same authority (LR-36): the list was open to any session.
	mux.Handle("GET /api/v1/runners/registration-tokens",
		s.auth.RequireSession(s.handleListRegistrationTokens(svc)))
	mux.Handle("POST /api/v1/runners/registration-tokens",
		s.requirePerm("configureApp", permConfigureApp)(s.handleMintRegistrationToken(svc)))
	mux.Handle("DELETE /api/v1/runners/registration-tokens/{id}",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRegistrationTokenAgency(http.HandlerFunc(svc.HandleRevokeRegistrationToken))))

	// ── Hand a Global-owned agent to the one agency it serves (MA-12) ─────
	// The only owner change there is. The owner gate asks for a global
	// administrator on the row this is for; the handler adds the target side.
	mux.Handle("POST /api/v1/runners/{id}/owner",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", http.HandlerFunc(s.handleSetRunnerOwner))))

	// ── The local runner's switch (LR-1, LR-17, LR-43) ────────────────────
	// Read by any session: the Runners view lists the row for everyone, and
	// the two facts this adds (whether it is turned on, whether the host
	// forbids it) explain why its jobs wait. Written by a global administrator.
	mux.Handle("GET /api/v1/local-runner", s.auth.RequireSession(http.HandlerFunc(s.handleGetLocalRunner)))
	mux.Handle("PUT /api/v1/local-runner", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleSetLocalRunner)))

	// ── Runner: long-poll for work (A6.2, doubles as heartbeat) ───────────
	// Path: GET /api/v1/runners/{id}/poll
	mux.Handle("GET /api/v1/runners/{id}/poll",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandlePoll)))

	// ── Drain (ConfigureApp + owning agency, RF-2 — see requireRunnerAgency) ──
	mux.Handle("POST /api/v1/runners/{id}/drain",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", http.HandlerFunc(svc.HandleDrain)))))

	// ── Resync (ConfigureApp — fleet op; runner-install-update.md Phase 4) ─
	// Sets resync_requested; the next poll delivers PollControl{Op:"re-register"}.
	// (The protocol >= 4 gate and its 409 went with the floor in v1.5.40: every
	// registered agent speaks the current protocol.)
	mux.Handle("POST /api/v1/runners/{id}/resync",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", http.HandlerFunc(svc.HandleResync)))))

	// ── Managed settings (ConfigureApp — fleet op; plan 2 Phase 4) ────────
	// Replaces the runner's server-managed override set and bumps the version;
	// the next poll delivers it to a settings-capable agent, applied in-memory.
	mux.Handle("PATCH /api/v1/runners/{id}/settings",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", http.HandlerFunc(svc.HandleUpdateRunnerSettings)))))

	// ── Secret-injection trust flag (ConfigureApp; vault-integration.md P1.4) ──
	// Operator grant permitting this runner to claim binding-bearing runs and
	// receive injected secret material in the manifest. Operator-set, never
	// self-declared.
	mux.Handle("PUT /api/v1/runners/{id}/secret-injection",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", http.HandlerFunc(svc.HandleSetSecretInjection)))))

	// ── Host-key scan & approve (plan 2 Phase 5, D4: 4A) ──────────────────
	// Operator: queue a scan (delivered as a v5 keyscan op), list pending scanned
	// keys, and approve/reject one (approval stages a trust-hosts op).
	// The scan, the per-runner pending list and the batch decision carry LR-63's
	// exception (requireRunnerOwnerOrHostKeyGuest); every other host-key route is
	// the owner's alone.
	hkGuest := func(h http.HandlerFunc) http.Handler {
		return s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerOwnerOrHostKeyGuest("id", s.agentOnly("id", h)))
	}
	mux.Handle("POST /api/v1/runners/{id}/keyscan", hkGuest(svc.HandleKeyscan))
	mux.Handle("GET /api/v1/runners/host-keys/pending",
		s.requirePerm("configureApp", permConfigureApp)(s.handleListPendingHostKeys(svc)))
	mux.Handle("POST /api/v1/runners/host-keys/{keyId}/resolve",
		s.requirePerm("configureApp", permConfigureApp)(s.requireHostKeyRunnerAgency(http.HandlerFunc(svc.HandleResolveHostKey))))

	// ── Host-key ledger (SB band; migrations 1200/1210, protocol 14) ───────
	// Everything that decides, or reveals, what ONE runner trusts carries that
	// runner's owner gate — reads included, unlike the rest of the runner
	// surface: the ledger names the hosts of the scopes a runner serves. A
	// deregistered runner's record is still its former owner's (runnerOwner
	// reads the snapshot taken at deregistration); with no snapshot left, or an
	// owner agency since deleted, it is a global administrator's.
	hk := func(h http.HandlerFunc) http.Handler {
		return s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", h)))
	}
	mux.Handle("GET /api/v1/runners/{id}/host-keys", hk(svc.HandleRunnerHostKeys))
	mux.Handle("GET /api/v1/runners/{id}/host-keys/pending", hkGuest(svc.HandleListRunnerPendingHostKeys))
	mux.Handle("POST /api/v1/runners/{id}/host-keys/resolve-batch", hkGuest(svc.HandleResolveHostKeyBatch))
	mux.Handle("POST /api/v1/runners/{id}/host-keys/provide", hk(svc.HandleProvideHostKeys))
	mux.Handle("POST /api/v1/runners/{id}/host-keys/{ledgerId}/remove", hk(svc.HandleRemoveHostKey))
	mux.Handle("POST /api/v1/runners/{id}/host-keys/{ledgerId}/resend", hk(svc.HandleResendHostKey))
	mux.Handle("POST /api/v1/runners/{id}/known-hosts/refresh", hk(svc.HandleRequestKnownHosts))
	// Carrying keys reads one runner's record and writes another's trust, so it
	// needs the gate for BOTH. The source rides the query (?from=) because a
	// second path wildcard here cannot be told apart from the older
	// /runners/host-keys/{keyId}/resolve.
	mux.Handle("POST /api/v1/runners/{id}/host-keys/carry",
		hk(func(w http.ResponseWriter, r *http.Request) {
			id, _ := auth.IdentityFrom(r.Context())
			if !s.requireRunnerOwner(w, r, id, r.URL.Query().Get("from")) {
				return
			}
			svc.HandleCarryHostKeys(w, r)
		}))
	mux.Handle("GET /api/v1/host-key-batches/{batchId}",
		s.requirePerm("configureApp", permConfigureApp)(s.requireHostKeyBatchRunnerAgency(http.HandlerFunc(svc.HandleHostKeyBatch))))
	// Which of a scope's hosts each bound runner trusts. The handler adds the
	// scope read check: host names are the scope's contents.
	mux.Handle("GET /api/v1/scopes/{scopeId}/host-key-coverage",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(svc.HandleScopeHostKeyCoverage)))
	// Runner: upload the keys it scanned (runner-key auth, ownership-guarded).
	// ET-D: the agent reports what it OBSERVED; the server decides what that
	// means and does the firing. Mirrors the host-key upload (v5) — same
	// bearer auth, same ownership guard, same "agent never acts, only
	// reports" shape.
	mux.Handle("POST /api/v1/runners/{id}/file-sightings",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleFileSightings)))
	mux.Handle("POST /api/v1/runners/{id}/hostkeys",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleUploadHostKeys)))
	// Runner: report its own known_hosts file (protocol 14).
	mux.Handle("POST /api/v1/runners/{id}/known-hosts",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleUploadKnownHosts)))

	// ── Runner: id-preserving re-declare (protocol v4) ────────────────────
	// Authenticated by the runner's own crn_run_* key (never a registration
	// token — D6); ownership guard inside the handler, like poll.
	mux.Handle("POST /api/v1/runners/{id}/redeclare",
		s.auth.RequireRunner(http.HandlerFunc(svc.HandleRedeclare)))

	// ── Accept a placement suggestion (DR-7 (c); MA-32) ───────────────────
	// ConfigureApp at the mount; the runner's owner gate is inside the handler.
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
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", http.HandlerFunc(svc.HandleDeregisterRunner)))))

	// ── Test Runner Connection (ConfigureApp + owning agency, RF-2) ───────
	mux.Handle("POST /api/v1/runners/{id}/test",
		s.requirePerm("configureApp", permConfigureApp)(s.requireRunnerAgency("id", s.agentOnly("id", http.HandlerFunc(svc.HandleTestRunner)))))

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

// handleAcceptRunnerPlacement accepts a placement suggestion for a re-enrolled
// runner (DR-7 (c), re-keyed by MA-32).
//
// DR-Q2 in one sentence: this endpoint is the human confirmation. The server
// never re-binds a re-registered runner on its own, because the name a
// suggestion is looked up by is self-declared by the agent.
//
// Authorisation is the runner's owner gate, and that is enough, because of what
// an accept can reach since 2.3.0: it re-points bindings on scopes of the
// runner's OWN agency (runner.ApplyPlacement), which is the agency the caller
// just proved they administer. It adds nothing to what the runner serves, so
// there is no target agency left to check.
func (s *Server) handleAcceptRunnerPlacement(w http.ResponseWriter, r *http.Request, svc *runner.Service) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireRunnerOwner(w, r, id, r.PathValue("id")) {
		return
	}
	// It never enrols, so it is never the runner an agent's old placement is
	// restored to.
	if !s.isAgent(w, r, r.PathValue("id")) {
		return
	}
	var body struct {
		HistoryID int64 `json:"historyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.HistoryID == 0 {
		httpx.Fail(w, http.StatusBadRequest, "invalid_body", "historyId is required")
		return
	}

	err := svc.ApplyPlacement(r.Context(), r.PathValue("id"), body.HistoryID, id.Email)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, runner.ErrPlacementTags):
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_tags",
			"restoring these tags would break the tag rules; trim the runner's tags and retry")
	case errors.Is(err, runner.ErrPlacementGone):
		httpx.Fail(w, http.StatusConflict, "placement_stale",
			"this suggestion no longer applies — the scopes may have been re-bound, or the runner removed or renamed, since")
	default:
		httpx.Fail500(w, s.log, "db_error", err)
	}
}

// handleDismissRunnerPlacement withdraws a suggestion (DRF-3). Gated exactly
// like accept: the runner's owner.
func (s *Server) handleDismissRunnerPlacement(w http.ResponseWriter, r *http.Request, svc *runner.Service) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireRunnerOwner(w, r, id, r.PathValue("id")) {
		return
	}
	// It never enrols, so it is never the runner an agent's old placement is
	// restored to.
	if !s.isAgent(w, r, r.PathValue("id")) {
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
			"this suggestion no longer exists, was already dismissed, or is not offered to this runner")
	case errors.Is(err, runner.ErrPlacementShared):
		httpx.Fail(w, http.StatusConflict, "placement_shared",
			"the previous runner is still bound to another agency's scopes, so this offer is theirs too and cannot "+
				"be dismissed from here; accept it, or re-bind your own scopes, and it goes away for this runner")
	default:
		httpx.Fail500(w, s.log, "db_error", err)
	}
}

// handleListRegistrationTokens lists the registration tokens the caller may
// administer (LR-36): those that enrol an agent for an agency the caller holds
// configureApp on. A global administrator sees every one.
//
// The list was open to any session until 2.3.0. A token's label is usually the
// host it is meant for and its creator is a person, and with agencies enrolling
// their own agents neither is every signed-in user's to read. A caller with no
// authority anywhere gets an empty list, not a refusal: the Runners view asks
// for it on every load.
func (s *Server) handleListRegistrationTokens(svc *runner.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		all, err := svc.RegistrationTokens(r.Context())
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		out := all[:0]
		for _, t := range all {
			if id.CanAgency(auth.PermConfigureApp, t.AgencyID) {
				out = append(out, t)
			}
		}
		httpx.JSON(w, http.StatusOK, out)
	}
}

// handleMintRegistrationToken settles WHOSE agent the token will enrol, checks
// the caller may enrol one for that agency, and hands the request on with the
// answer written into it (LR-61).
//
// The owner follows the rule every creation follows (requireRecordOwnerChoice):
// the agency named, if the caller administers it; with none named, the caller's
// only agency; and Global for a global administrator who names none. An
// administrator of several agencies must say which.
func (s *Server) handleMintRegistrationToken(svc *runner.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		// The body is optional and small: a label and an agency. Read here so the
		// agency can be authorized before anything is minted.
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			httpx.Fail(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
			return
		}
		// Decoded into the service's OWN request type, by the same decoder, and
		// what goes on to the service is that value re-encoded and nothing of
		// the caller's bytes. The agency that is authorized here must be the
		// agency that is stored there, and two readings of one body are two
		// chances to disagree: encoding/json matches a field name whatever its
		// letter case and lets the last match win, so a body that carried
		// "agencyid" beside the "agencyId" this handler looked up would have
		// been authorized for one agency and minted for the other.
		var req runner.MintRequest
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"request body must be a JSON object (optional: {\"label\": \"...\", \"agencyId\": \"...\"})")
				return
			}
		}
		owner, ok := s.requireRecordOwnerChoice(w, r, id, strings.TrimSpace(req.AgencyID), "runner")
		if !ok {
			return
		}
		req.AgencyID = owner
		replay, _ := json.Marshal(req)
		r.Body = io.NopCloser(bytes.NewReader(replay))
		svc.HandleMintRegistrationToken(w, r)
	}
}

// requireRegistrationTokenAgency gates a revoke by the agency the token would
// enrol an agent for (LR-36). A token id that matches nothing falls through to
// the handler's own 404.
func (s *Server) requireRegistrationTokenAgency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		rowID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			next.ServeHTTP(w, r) // the handler's 404
			return
		}
		agency, found, err := s.runnerSvc.RegistrationTokenAgency(r.Context(), rowID)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if found && !id.CanAgency(auth.PermConfigureApp, agency) {
			if agency == agencyid.Global {
				s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
					"this token enrols a runner for Global, so only a global administrator may revoke it")
				return
			}
			s.denyEntityAgency(w, r, id, auth.PermConfigureApp, agency,
				"you do not have "+auth.PermConfigureApp+" on the agency this token enrols a runner for")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleSetRunnerOwner hands a Global-owned agent to the one agency it serves
// (MA-12, "Hand to an agency"). It is the only owner change there is. The route
// has already asked for authority over the runner as it stands; this adds the
// agency it is going to.
func (s *Server) handleSetRunnerOwner(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body struct {
		AgencyID string `json:"agencyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AgencyID == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "agencyId is required")
		return
	}
	if !s.requireKnownAgency(w, r, body.AgencyID) {
		return
	}
	if !id.CanAgency(auth.PermConfigureApp, body.AgencyID) {
		s.denyEntityAgency(w, r, id, auth.PermConfigureApp, body.AgencyID,
			"you do not have "+auth.PermConfigureApp+" on agency "+body.AgencyID+", so you cannot hand this runner to it")
		return
	}
	err := settings.SetRunnerOwner(r.Context(), s.db, r.PathValue("id"), body.AgencyID, id.Email)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, settings.ErrUnknownRunner):
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
	case failAgencyRule(w, err):
	default:
		httpx.Fail500(w, s.log, "update_failed", err)
	}
}

func (s *Server) handleGetLocalRunner(w http.ResponseWriter, r *http.Request) {
	lr, err := settings.GetLocalRunner(r.Context(), s.db, s.cfg.LocalRunnerForbid)
	switch {
	case errors.Is(err, settings.ErrNoLocalRunner):
		httpx.Fail(w, http.StatusNotFound, "not_found", err.Error())
	case err != nil:
		httpx.Fail500(w, s.log, "db_error", err)
	default:
		httpx.JSON(w, http.StatusOK, lr)
	}
}

// handleSetLocalRunner turns the local runner on or off and sets its
// concurrency (LR-43, LR-44). The stored state changes first, in a transaction
// with its audit row; the engine is then told, which starts the claim loop or
// lets it drain. If the engine cannot be told (it reads the same stored state
// at the next start), the answer says so rather than claiming it took effect.
func (s *Server) handleSetLocalRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body struct {
		Enabled       *bool `json:"enabled"`
		MaxConcurrent *int  `json:"maxConcurrent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if body.Enabled == nil && body.MaxConcurrent == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "give enabled, maxConcurrent, or both")
		return
	}
	lr, err := settings.SetLocalRunner(r.Context(), s.db, s.cfg.LocalRunnerForbid, body.Enabled, body.MaxConcurrent, id.Email)
	switch {
	case errors.Is(err, settings.ErrLocalRunnerForbidden):
		httpx.Fail(w, http.StatusConflict, "local_runner_forbidden", err.Error())
		return
	case errors.Is(err, settings.ErrNoLocalRunner):
		httpx.Fail(w, http.StatusNotFound, "not_found", err.Error())
		return
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	case err != nil:
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if s.sshExec != nil {
		// The engine outlives this request: it must not start its claim loop
		// on a context that ends when the response is written.
		if err := s.sshExec.Apply(context.WithoutCancel(r.Context())); err != nil {
			s.log.Error("local runner: the setting was saved and the engine could not apply it", "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "apply_failed",
				"the setting was saved, but the running server could not apply it; it takes effect at the next restart")
			return
		}
		if fresh, ferr := settings.GetLocalRunner(r.Context(), s.db, s.cfg.LocalRunnerForbid); ferr == nil {
			lr = fresh // the status the engine just wrote
		}
	}
	httpx.JSON(w, http.StatusOK, lr)
}
