package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// HandleResync flags a runner for re-declaration (Phase 4, D2 stage 1).
// Operator, CSRF required. The next poll from the runner delivers
// PollControl{Op:"re-register"} and clears the flag; the agent then
// re-declares its current local config in place via HandleRedeclare —
// same id, same API key, no deregister dance.
//
// Security note: resync cannot be used to change WHAT a runner is — the op
// makes the agent re-read its own local config; the server (or a compromised
// operator session) still can't grant itself capabilities on a runner.
// POST /api/v1/runners/{id}/resync
func (s *Service) HandleResync(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	var row runnerRow
	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, status, os, capabilities, load, max_concurrent, version,
		       inventory, protocol_version, registered_at
		FROM runners WHERE id = ?`, runnerID).Scan(
		&row.ID, &row.Name, &row.Status, &row.OS, &row.Capabilities, &row.Load,
		&row.MaxConcurrent, &row.Version, &row.Inventory, &row.ProtocolVersion,
		&row.RegisteredAt)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE runners SET resync_requested = 1 WHERE id = ?`, runnerID); err != nil {
		s.log.Error("set resync_requested", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// Audit entry (same shape as drain).
	ts := now()
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      sessionActor(r),
		Target:     "runner:" + row.Name,
		RunnerName: row.Name,
		Summary:    "resync requested",
	})

	resp, _ := rowToResponse(row, s.cfg.RunnerOfflineAfter)
	httpx.JSON(w, http.StatusAccepted, resp)
}

// checkConfigDrift reports whether the digest the agent sent with this poll
// differs from the stored declared-config digest (Phase 5, D2 stage 2) and a
// re-register op should be delivered. An empty agentDigest (an agent that
// sends no digest) never drifts. An empty STORED digest — a pre-550 row —
// counts as drift and self-heals via one redeclare.
//
// The flap guard bounds delivery to one op per driftOpCooldown per runner: an
// agent that keeps polling with a mismatched digest after a redeclare (a bug,
// or two agents sharing one identity) must not re-register-loop. Suppressed
// repeats are logged loudly.
func (s *Service) checkConfigDrift(runnerID string, agentDigest, storedDigest string) bool {
	if agentDigest == "" || agentDigest == storedDigest {
		return false
	}
	s.driftMu.Lock()
	defer s.driftMu.Unlock()
	if last, ok := s.driftLastOp[runnerID]; ok && time.Since(last) < driftOpCooldown {
		s.log.Warn("config drift persists within the re-register cooldown — agent may be failing to redeclare (flap guard active)",
			"runner_id", runnerID, "agent_digest", agentDigest, "stored_digest", storedDigest)
		return false
	}
	s.driftLastOp[runnerID] = time.Now()
	s.log.Info("declared-config drift detected — requesting re-register on this poll",
		"runner_id", runnerID)
	return true
}

// noteReRegisterSent records an op delivery so the drift guard's cooldown also
// covers ops triggered by the manual Resync button (which itself bypasses the
// cooldown — an operator click always delivers).
func (s *Service) noteReRegisterSent(runnerID string) {
	s.driftMu.Lock()
	s.driftLastOp[runnerID] = time.Now()
	s.driftMu.Unlock()
}

// takeResync atomically consumes a pending resync request for this runner.
// Returns true when the flag was set AND this call cleared it — the caller
// must deliver the "re-register" op on THIS poll response (deliver-once, like
// drainControl).
func (s *Service) takeResync(ctx context.Context, runnerID string) bool {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runners SET resync_requested = 0
		WHERE id = ? AND resync_requested = 1`, runnerID)
	if err != nil {
		s.log.Error("consume resync_requested", "runner_id", runnerID, "error", err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false
	}
	return n == 1
}

// HandleRedeclare is the id-preserving re-registration (Phase 4, protocol v4).
// The agent calls it on receipt of the "re-register" control op, authenticated
// with its EXISTING crn_run_* runner key — never a registration token (D6:
// single-use registration tokens are first-contact credentials and are dead by
// resync time). The existing row is updated in place: same id, same agency
// membership, same API key, no orphan row. The reaped/404 poll path keeps its
// token-based fresh registration (agent.go).
// POST /api/v1/runners/{id}/redeclare
func (s *Service) HandleRedeclare(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	// Ownership (R1.4): the presented runner bearer must own {id}. 404 (not 403)
	// so a non-owning runner cannot probe ids — consistent with HandlePoll.
	callerRunnerID, ok := auth.RunnerIDFrom(r.Context())
	if !ok || callerRunnerID != runnerID {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"request body must be valid JSON")
		return
	}
	if msg := req.normalizeAndValidate(); msg != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", msg)
		return
	}
	// Same floor as register: the redeclare is the same handshake.
	if req.ProtocolVersion < runnerproto.MinProtocolVersion {
		httpx.Fail(w, http.StatusUpgradeRequired, "protocol_too_old",
			fmt.Sprintf("redeclare requires protocol %d (agent declared %d); upgrade the agent",
				runnerproto.MinProtocolVersion, req.ProtocolVersion))
		return
	}
	if req.MaxConcurrent <= 0 {
		req.MaxConcurrent = 5
	}

	capsJSON, _ := json.Marshal(req.Capabilities)
	var toolchainsVal any // NULL when absent
	if len(req.Toolchains) > 0 && string(req.Toolchains) != "null" {
		toolchainsVal = string(req.Toolchains)
	}
	ts := now()

	// The refreshed declared-config digest (Phase 5) — stored alongside the
	// declared values so the drift check converges on the next poll.
	configDigest := runnerproto.ConfigDigest(req.Name, req.OS, req.Capabilities,
		req.MaxConcurrent, req.Inventory, req.Version, req.ProtocolVersion)

	// In-place update: status/load/agencies/tokens/registration provenance are
	// deliberately untouched — this re-declares config, it does not re-admit the
	// runner. registered_at moves to now: it reads as "declared as of", exactly
	// what the old deregister/re-register dance produced.
	res, err := s.db.ExecContext(r.Context(), `
		UPDATE runners
		SET name = ?, os = ?, capabilities = ?, max_concurrent = ?, version = ?,
		    inventory = ?, toolchains = ?, protocol_version = ?, config_digest = ?,
		    registered_at = ?, last_seen_at = ?, resync_requested = 0
		WHERE id = ?`,
		req.Name, req.OS, string(capsJSON), req.MaxConcurrent, req.Version,
		req.Inventory, toolchainsVal, req.ProtocolVersion, configDigest, ts, ts, runnerID)
	if err != nil {
		s.log.Error("redeclare runner", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Row vanished (reaped/deregistered) between auth and update: the agent's
		// next poll 404s and takes the token-based fresh-registration path.
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// Audit entry: the redeclare is agent-initiated (operator intent was already
	// logged by HandleResync when the trigger was the Resync button).
	// Actor is the runner's id and target its (possibly new) name — the split is
	// deliberate: the agent acted on itself, and the name may have just changed.
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      "runner:" + runnerID,
		Target:     "runner:" + req.Name,
		RunnerName: req.Name,
		Summary:    "re-declared configuration (resync)",
	})

	var caps []string
	_ = json.Unmarshal(capsJSON, &caps)
	var respToolchains json.RawMessage
	if toolchainsVal != nil {
		respToolchains = json.RawMessage(toolchainsVal.(string))
	}
	// Status is whatever it was; report it back accurately.
	var status string
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT status FROM runners WHERE id = ?`, runnerID).Scan(&status)

	httpx.JSON(w, http.StatusOK, runnerResponse{
		ID:              runnerID,
		Name:            req.Name,
		Status:          status,
		OS:              req.OS,
		Capabilities:    caps,
		MaxConcurrent:   req.MaxConcurrent,
		Version:         req.Version,
		Inventory:       req.Inventory,
		ProtocolVersion: req.ProtocolVersion,
		RegisteredAt:    ts,
		Toolchains:      respToolchains,
	})
}
