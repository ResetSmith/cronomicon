package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Server-managed runner settings (runner-install-update-2.md Phase 4, D3/D5).
//
// The operator edits operational knobs on a runner's row; they are stored in
// runners.managed_settings (a tri-state JSON object — a present key overrides
// the agent's local value, an absent key means "no opinion"), versioned by
// settings_version, and delivered to the agent over the poll channel until it
// acks (settings_acked_version). The stored JSON shares runnerproto's field
// tags, so it round-trips through PollSettingsValues directly.

// runTypeVocab is the closed run-type set a capabilityMask may narrow (mirrors
// the openapi RunType enum / the agent's detectRunTypes tokens).
var runTypeVocab = map[string]bool{
	"bash": true, "perl": true, "powershell": true,
	"python": true, "ansible": true, "terraform": true,
}

// parseManagedSettings decodes runners.managed_settings into the wire value
// shape. Empty/NULL ⇒ the zero value (no overrides).
func parseManagedSettings(raw string) (runnerproto.PollSettingsValues, error) {
	var v runnerproto.PollSettingsValues
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return v, nil
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, err
	}
	return v, nil
}

// effectiveClaimCaps subtracts a capability mask from a runner's declared
// capability tokens — the server-side enforcement of the subtract-only mask.
// Order-preserving; a nil/empty mask returns caps unchanged.
func effectiveClaimCaps(caps []string, mask []string) []string {
	if len(mask) == 0 {
		return caps
	}
	masked := make(map[string]bool, len(mask))
	for _, m := range mask {
		masked[m] = true
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if !masked[c] {
			out = append(out, c)
		}
	}
	return out
}

// validateAndNormalize checks an incoming managed-settings object and returns
// its canonical JSON for storage. An all-empty object stores as "" (NULL) so
// the poll layer reports "no server opinion".
func validateAndNormalize(v runnerproto.PollSettingsValues) (string, error) {
	if v.MaxConcurrent != nil && *v.MaxConcurrent <= 0 {
		return "", fmt.Errorf("maxConcurrent must be > 0")
	}
	trimEmpty := func(p *string, name string) error {
		if p != nil && strings.TrimSpace(*p) == "" {
			return fmt.Errorf("%s must not be empty when set (omit it to clear)", name)
		}
		return nil
	}
	if err := trimEmpty(v.SandboxMemoryMax, "sandboxMemoryMax"); err != nil {
		return "", err
	}
	if err := trimEmpty(v.SandboxCPUQuota, "sandboxCpuQuota"); err != nil {
		return "", err
	}
	if err := trimEmpty(v.SandboxTasksMax, "sandboxTasksMax"); err != nil {
		return "", err
	}
	if v.CheckoutRepos != nil {
		for _, u := range *v.CheckoutRepos {
			if strings.TrimSpace(u) == "" {
				return "", fmt.Errorf("checkoutRepos entries must not be empty")
			}
		}
	}
	for _, m := range v.CapabilityMask {
		if !runTypeVocab[m] {
			return "", fmt.Errorf("capabilityMask entry %q is not a run-type", m)
		}
	}

	// All-nil ⇒ no opinion; store NULL.
	if v.MaxConcurrent == nil && v.SandboxMemoryMax == nil && v.SandboxCPUQuota == nil &&
		v.SandboxTasksMax == nil && v.AllowCheckout == nil && v.CheckoutRepos == nil &&
		len(v.CapabilityMask) == 0 {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// updateSettingsRequest is the PATCH body: the COMPLETE desired managed set
// (present fields are managed, absent fields are cleared) — a replace, not a
// merge, so the drawer's "unset" states clear cleanly.
type updateSettingsRequest struct {
	runnerproto.PollSettingsValues
}

// updateSettingsResponse is returned on a successful PATCH so the UI can reflect
// the new version + pending-ack state without a full refetch.
type updateSettingsResponse struct {
	ManagedSettings      json.RawMessage `json:"managedSettings"`
	SettingsVersion      int             `json:"settingsVersion"`
	SettingsAckedVersion int             `json:"settingsAckedVersion"`
}

// HandleUpdateRunnerSettings replaces a runner's server-managed settings and
// bumps its version (delivered on the next poll). Operator, CSRF required.
// PATCH /api/v1/runners/{id}/settings
func (s *Service) HandleUpdateRunnerSettings(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	var req updateSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	stored, err := validateAndNormalize(req.PollSettingsValues)
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	var name string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// Bump the version so the change is redelivered until the agent acks. Store
	// NULL for an all-empty set (no server opinion).
	var storedArg any
	if stored == "" {
		storedArg = nil
	} else {
		storedArg = stored
	}
	var newVersion, acked int
	if err := s.db.QueryRowContext(r.Context(), `
		UPDATE runners SET managed_settings = ?, settings_version = settings_version + 1
		WHERE id = ?
		RETURNING settings_version, settings_acked_version`, storedArg, runnerID).
		Scan(&newVersion, &acked); err != nil {
		s.log.Error("update runner settings", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	if idn, ok := auth.IdentityFrom(r.Context()); ok {
		_ = settings.WriteChangeLog(r.Context(), s.db, idn.Email, "Runners", "settings updated",
			name, fmt.Sprintf("version %d", newVersion))
	}

	var raw json.RawMessage
	if stored != "" {
		raw = json.RawMessage(stored)
	}
	httpx.JSON(w, http.StatusOK, updateSettingsResponse{
		ManagedSettings:      raw,
		SettingsVersion:      newVersion,
		SettingsAckedVersion: acked,
	})
}
