package runner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
)

// runnerRow is the internal DB representation of a runner.
type runnerRow struct {
	ID                  string
	Name                string
	Status              string
	OS                  string
	Capabilities        string // JSON []string
	Load                int
	MaxConcurrent       int
	Version             string
	Inventory           string
	Toolchains          *string // JSON detail (RX.7), display-only; NULL for pre-Phase-4 agents
	ProtocolVersion     int
	LastSeenAt          *string
	DrainDeadlineAt     *string
	RegisteredAt        string
	CreatedAt           string
	RegistrationTokenID *int64
	// Server-managed settings (Phase 4): JSON blob of tri-state overrides + the
	// version bumped on write and the version the agent last acked.
	ManagedSettings      *string // JSON; NULL = no server opinion
	SettingsVersion      int
	SettingsAckedVersion int
	// Tags is the operator-authored tag set (migration 580): a JSON []string,
	// SQLite-only, set via PUT /runner-tags/{runnerId}. Defaults to '[]'.
	Tags string
	// AllowSecretInjection is the operator trust flag (migration 600): when set,
	// this runner may claim runs whose job/script declares reference bindings and
	// receive injected secret material in the manifest (P1.4). Defaults false.
	AllowSecretInjection bool
}

// agencyRef is the lightweight {id,name} of an agency a runner belongs to (M2).
type agencyRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// runnerResponse is the JSON shape for the Runner schema in openapi.yaml.
type runnerResponse struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	OS            string   `json:"os"`
	Capabilities  []string `json:"capabilities"`
	Load          int      `json:"load"`
	MaxConcurrent int      `json:"maxConcurrent"`
	Version       string   `json:"version"`
	// Inventory is the per-runner inventory canonicality (D8): 'cronomicon' (the
	// manifest carries fully-resolved targets) or 'local' (the agent resolves
	// hosts against its own inventory). Defaults to 'cronomicon'.
	Inventory string `json:"inventory"`
	// ProtocolVersion is the runner↔server wire-protocol version the agent
	// declared at its last (re-)registration (migration 390). The Runners view
	// gates the Resync button on >= 4 (runner-install-update.md Phase 4).
	// omitempty: 0 = a pre-handshake agent that never declared one.
	ProtocolVersion     int     `json:"protocolVersion,omitempty"`
	RegisteredAt        string  `json:"registeredAt"`
	LastHeartbeatAt     *string `json:"lastHeartbeatAt"`
	DrainDeadlineAt     *string `json:"drainDeadlineAt,omitempty"`
	RegistrationTokenID *int64  `json:"registrationTokenId,omitempty"`
	// Agencies is the runner's network-isolation membership (agency-support.md M2),
	// operator-assigned (never self-declared). Populated by the list serializer.
	Agencies []agencyRef `json:"agencies,omitempty"`
	// DR-7: present only on an UNBOUND runner for which a prior placement was
	// captured. Absent means "no offer", never "no placement".
	PlacementSuggestion *placementSuggestion `json:"placementSuggestion,omitempty"`
	// Toolchains is the detected-toolchain display detail (RX.7): ansible-core
	// version, installed collections + versions, checkout/vault flags. Raw JSON
	// passthrough; omitted for a runner that never reported it.
	Toolchains json.RawMessage `json:"toolchains,omitempty"`
	// ManagedSettings is the server-managed operational override set (Phase 4):
	// a tri-state JSON object edited from the runner's Settings drawer. Omitted
	// when the server has no opinion. SettingsVersion > SettingsAckedVersion
	// means a change is still propagating to the agent (pending-ack).
	ManagedSettings      json.RawMessage `json:"managedSettings,omitempty"`
	SettingsVersion      int             `json:"settingsVersion,omitempty"`
	SettingsAckedVersion int             `json:"settingsAckedVersion,omitempty"`
	// Tags is the operator-authored tag set (migration 580), SQLite-only, edited
	// from the runner's expanded row. Always present ([] when none).
	Tags []string `json:"tags"`
	// AllowSecretInjection is the operator trust flag (migration 600) permitting
	// this runner to receive injected secret material (P1.4). Edited from the
	// runner's expanded row; defaults false.
	AllowSecretInjection bool `json:"allowSecretInjection"`
}

// degradedAfter is the point at which a still-online runner stops being trusted
// as current: more than this without a heartbeat and it is reported `degraded`.
// Agents poll once a minute (D7), so two missed polls is the signal — early
// enough to be worth showing, late enough not to flag ordinary jitter.
const degradedAfter = 2 * time.Minute

// deriveStatus is FX-18: the `degraded` runner state, computed at READ time.
//
// The gap it closes is real and was invisible. `last_seen_at` is written on every
// long-poll and the reaper only flips a runner to `offline` after
// cfg.RunnerOfflineAfter (default 5m), so for up to five minutes a runner that
// has already died read **Online** — in its row, in the tile counts, and to an
// operator deciding where to send work. The OpenAPI enum has always promised
// `degraded` and the UI has always drawn a Degraded tile; nothing ever produced
// it, so the tile read 0 forever.
//
// Derived rather than stored, deliberately: the stored status is the runner's
// LIFECYCLE (what register/drain/reaper transition it through) and this is a
// presentation of freshness on top of it. Storing it would mean widening the
// `CHECK (status IN (...))` constraint — a SQLite table rebuild — and teaching
// the reaper's `WHERE status IN ('online','draining')` sweep about the new value,
// or a degraded runner would never be offlined or deregistered at all.
//
// Only `online` degrades: `draining` is already leaving, and `offline` has
// already been reaped.
func deriveStatus(stored string, lastSeenAt *string, offlineAfter time.Duration) string {
	if stored != "online" {
		return stored
	}
	if lastSeenAt == nil || *lastSeenAt == "" {
		// Registered but never polled: not fresh, but the reaper owns calling it
		// offline. Degraded is the honest middle.
		return "degraded"
	}
	seen, err := time.Parse(time.RFC3339, *lastSeenAt)
	if err != nil {
		// Unparseable heartbeat — the reaper treats this as stale rather than
		// trusting it (reaper.go), and so does this.
		return "degraded"
	}
	age := statusNow().Sub(seen)
	// Past the offline threshold the reaper is about to take over; reporting
	// `degraded` there is still truer than `online`.
	if age > degradedAfter || age > offlineAfter {
		return "degraded"
	}
	return stored
}

// statusNow is a clock seam for tests, mirroring reaperNow.
var statusNow = func() time.Time { return time.Now().UTC() }

func rowToResponse(r runnerRow, offlineAfter time.Duration) (runnerResponse, error) {
	var caps []string
	if err := json.Unmarshal([]byte(r.Capabilities), &caps); err != nil {
		caps = nil
	}
	var toolchains json.RawMessage
	if r.Toolchains != nil && *r.Toolchains != "" {
		toolchains = json.RawMessage(*r.Toolchains)
	}
	var managed json.RawMessage
	if r.ManagedSettings != nil && *r.ManagedSettings != "" {
		managed = json.RawMessage(*r.ManagedSettings)
	}
	tags := []string{}
	if r.Tags != "" {
		if err := json.Unmarshal([]byte(r.Tags), &tags); err != nil || tags == nil {
			tags = []string{}
		}
	}
	return runnerResponse{
		ID:                   r.ID,
		Name:                 r.Name,
		Status:               deriveStatus(r.Status, r.LastSeenAt, offlineAfter),
		OS:                   r.OS,
		Capabilities:         caps,
		Load:                 r.Load,
		MaxConcurrent:        r.MaxConcurrent,
		Version:              r.Version,
		Inventory:            r.Inventory,
		ProtocolVersion:      r.ProtocolVersion,
		RegisteredAt:         r.RegisteredAt,
		LastHeartbeatAt:      r.LastSeenAt,
		DrainDeadlineAt:      r.DrainDeadlineAt,
		RegistrationTokenID:  r.RegistrationTokenID,
		Toolchains:           toolchains,
		ManagedSettings:      managed,
		SettingsVersion:      r.SettingsVersion,
		SettingsAckedVersion: r.SettingsAckedVersion,
		Tags:                 tags,
		AllowSecretInjection: r.AllowSecretInjection,
	}, nil
}

// HandleSetSecretInjection toggles a runner's operator secret-injection trust
// flag (migration 600, P1.4). PUT /api/v1/runners/{id}/secret-injection with body
// {"allow": bool}. Gated permConfigureApp + CSRF at the route. This is an operator
// grant, never agent-self-declared — mirroring agency membership.
func (s *Service) HandleSetSecretInjection(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	var req struct {
		Allow bool `json:"allow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", "request body is not valid JSON")
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE runners SET allow_secret_injection = ? WHERE id = ?`, req.Allow, runnerID)
	if err != nil {
		s.log.Error("set runner secret injection", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	var name string
	_ = s.db.QueryRowContext(r.Context(), `SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name)
	if idn, ok := auth.IdentityFrom(r.Context()); ok {
		detail := "secret injection disabled"
		if req.Allow {
			detail = "secret injection enabled"
		}
		_ = settings.WriteChangeLog(r.Context(), s.db, idn.Email, "Runners", "updated", name, detail)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"id": runnerID, "allowSecretInjection": req.Allow})
}

// HandleListRunners lists all registered runners (operator).
// GET /api/v1/runners
func (s *Service) HandleListRunners(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, status, os, capabilities, load, max_concurrent, version,
		       inventory, toolchains, protocol_version, last_seen_at, drain_deadline_at,
		       registered_at, created_at, registration_token_id,
		       managed_settings, settings_version, settings_acked_version, tags,
		       allow_secret_injection
		FROM runners ORDER BY registered_at DESC`)
	if err != nil {
		s.log.Error("list runners", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer rows.Close()

	var out []runnerResponse
	for rows.Next() {
		var row runnerRow
		var regTokID *int64
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Status, &row.OS,
			&row.Capabilities, &row.Load, &row.MaxConcurrent, &row.Version,
			&row.Inventory, &row.Toolchains, &row.ProtocolVersion, &row.LastSeenAt,
			&row.DrainDeadlineAt, &row.RegisteredAt, &row.CreatedAt, &regTokID,
			&row.ManagedSettings, &row.SettingsVersion, &row.SettingsAckedVersion, &row.Tags,
			&row.AllowSecretInjection,
		); err != nil {
			s.log.Error("scan runner row", "error", err)
			continue
		}
		row.RegistrationTokenID = regTokID
		resp, err := rowToResponse(row, s.cfg.RunnerOfflineAfter)
		if err != nil {
			continue
		}
		out = append(out, resp)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("list runners rows", "error", err)
	}
	rows.Close()

	// Attach agency membership (M2) with ONE grouped query — never a per-row nested
	// cursor (SQLite single-conn deadlock). Best-effort: a pre-440 schema yields none.
	byRunner := map[string][]agencyRef{}
	if arows, aerr := s.db.QueryContext(r.Context(), `
		SELECT ra.runner_id, a.id, a.name
		FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
		ORDER BY a.name`); aerr == nil {
		for arows.Next() {
			var rid string
			var ar agencyRef
			if err := arows.Scan(&rid, &ar.ID, &ar.Name); err == nil {
				byRunner[rid] = append(byRunner[rid], ar)
			}
		}
		arows.Close()
	}
	for i := range out {
		out[i].Agencies = byRunner[out[i].ID]
	}

	// DR-7 (c): offer a prior placement to every UNBOUND runner. Unbound is not
	// "idle" — the claim predicate's general-pool branch admits a runner with no
	// agencies to every UNTAGGED run (poll.go, AG-Q3a) — so a runner that lost its
	// placement has silently moved from its department's pool into the shared one.
	// That is an isolation change, which is why it is surfaced rather than left to
	// be noticed.
	unbound := map[string]string{}
	for i := range out {
		if len(out[i].Agencies) == 0 {
			unbound[out[i].ID] = out[i].Name
		}
	}
	if sugg, serr := suggestionsFor(r.Context(), s.db, unbound); serr != nil {
		// Best-effort: a suggestion is an aid, and losing it must not cost an
		// operator the runner list itself.
		s.log.Error("resolve placement suggestions", "error", serr)
	} else {
		for i := range out {
			out[i].PlacementSuggestion = sugg[out[i].ID]
		}
	}

	// Server-side tag filter (?tag=, ?tags=, ?tagMatch=), applied after tags are
	// decoded to a slice on the response — exact-element match, shared with every
	// other tagged list endpoint via tagutil.
	if tagFilter := tagutil.ParseQuery(r.URL.Query()); !tagFilter.Empty() {
		kept := make([]runnerResponse, 0, len(out))
		for _, resp := range out {
			if tagFilter.Match(resp.Tags) {
				kept = append(kept, resp)
			}
		}
		out = kept
	}

	if out == nil {
		out = []runnerResponse{}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// registerRequest is the JSON body for POST /runners/register.
type registerRequest struct {
	Name          string   `json:"name"`
	OS            string   `json:"os"`
	Capabilities  []string `json:"capabilities"`
	Version       string   `json:"version"`
	MaxConcurrent int      `json:"maxConcurrent"`
	// Inventory selects the per-runner inventory canonicality (D8):
	// 'cronomicon' (default) or 'local'. Optional; absent ⇒ 'cronomicon'.
	Inventory string `json:"inventory"`
	// ProtocolVersion is the runner↔server wire-protocol version the agent
	// speaks (R0.2). REQUIRED: absent decodes as zero and is refused with the
	// same 426 as any value below runnerproto.MinProtocolVersion. The
	// pre-handshake carve-out that once accepted zero with a warning went in
	// v1.5.40 — with the per-feature gates deleted, registration is one of the
	// three places the wire shape is checked at all (the others are redeclare
	// and every poll).
	ProtocolVersion int `json:"protocolVersion"`
	// Toolchains is the detected-toolchain display detail (RX.7), stored verbatim
	// on runners.toolchains for the Runners page. Optional; absent ⇒ NULL.
	Toolchains json.RawMessage `json:"toolchains,omitempty"`
}

// normalizeAndValidate applies defaults (inventory, implicitly maxConcurrent's
// caller-side default) and validates the declared-config fields shared by
// register and redeclare (Phase 4). Returns "" when valid, else the
// validation_failed message. Protocol-version policy is NOT here — register
// warns on absent/zero (pre-handshake back-compat) while redeclare rejects it
// (only v4+ agents call redeclare), so each handler applies its own.
func (req *registerRequest) normalizeAndValidate() string {
	if req.Name == "" || req.OS == "" || len(req.Capabilities) == 0 || req.Version == "" {
		return "name, os, capabilities, and version are required"
	}
	if req.OS != "Linux" && req.OS != "Windows" {
		return "os must be 'Linux' or 'Windows'"
	}
	// Inventory canonicality (D8): default 'cronomicon', validate the closed set.
	if req.Inventory == "" {
		req.Inventory = "cronomicon"
	}
	if req.Inventory != "cronomicon" && req.Inventory != "local" {
		return "inventory must be 'cronomicon' or 'local'"
	}
	return ""
}

// HandleRegisterRunner authenticates with a registration token — single-use
// per install since Phase 7 (D6), or the multi-use env bootstrap token — and
// creates a runners row, issuing a long-lived runner API key. (A6.1)
// The token row is consumed ATOMICALLY inside the registration transaction,
// so two hosts cannot register from one token.
// POST /api/v1/runners/register
func (s *Service) HandleRegisterRunner(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return
	}
	chk, err := s.checkRegistrationToken(r.Context(), token)
	if err != nil {
		s.log.Error("validate registration token", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "auth check failed")
		return
	}
	if !chk.OK {
		httpx.Fail(w, http.StatusUnauthorized, chk.Code, chk.Msg)
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

	// Protocol-version handshake (R0.2). The floor tracks the current protocol
	// (runnerproto.MinProtocolVersion — see its comment): an agent that declares
	// anything older, or nothing at all, is rejected loudly and actionably —
	// better a clear 426 at registration than silent wire drift mid-run. There
	// is no pre-handshake carve-out any more: with the per-feature gates gone,
	// registration is the ONE place the wire shape is checked.
	if req.ProtocolVersion < runnerproto.MinProtocolVersion {
		httpx.Fail(w, http.StatusUpgradeRequired, "protocol_too_old",
			fmt.Sprintf("runner protocol version %d too old (absent counts as 0); server requires %d; upgrade the agent",
				req.ProtocolVersion, runnerproto.MinProtocolVersion))
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
	runnerID := db.NewID()

	// Mint the per-runner long-lived API key (A6.1).
	apiKey, err := generateToken(runnerTokenPrefix)
	if err != nil {
		s.log.Error("generate runner api key", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "failed to generate api key")
		return
	}

	// Provenance: the matched registration_tokens row (nil for the bootstrap
	// token). The same row is consumed below, inside the transaction.
	var regTokID *int64
	if chk.RowID != 0 {
		regTokID = &chk.RowID
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// The declared-config digest (Phase 5): computed from the SAME normalized
	// values being stored, with the shared canonicalization the agent uses —
	// a later poll-time mismatch means the agent's local config drifted.
	configDigest := runnerproto.ConfigDigest(req.Name, req.OS, req.Capabilities,
		req.MaxConcurrent, req.Inventory, req.Version, req.ProtocolVersion)

	// Insert runners row.
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO runners(id, name, status, os, capabilities, load,
		                    max_concurrent, version, inventory, toolchains, protocol_version, config_digest,
		                    registered_at, created_at, registration_token_id, last_client_ip)
		VALUES (?, ?, 'online', ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runnerID, req.Name, req.OS, string(capsJSON),
		req.MaxConcurrent, req.Version, req.Inventory, toolchainsVal, req.ProtocolVersion, configDigest, ts, ts, regTokID,
		// DR-Q7: the one signal a re-registering agent cannot freely assert.
		// Resolved through the trusted-proxy allowlist — NEVER r.RemoteAddr,
		// which behind a reverse proxy is the proxy for every runner.
		nullStrOrNil(httpx.ClientIP(r, s.cfg.TrustedProxyNets())),
	); err != nil {
		s.log.Error("insert runner", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// Store the per-runner API key hash in runner_tokens (365d expiry — runners
	// use long-lived keys so this is refreshed on re-registration). The token is
	// bound to its owning runner_id (R1.4) so the manifest + log endpoints can
	// authorize by run ownership.
	apiKeyExpiry := time.Now().UTC().Add(365 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO runner_tokens(token_hash, runner_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		hashToken(apiKey), runnerID, "runner:"+req.Name, ts, apiKeyExpiry,
	); err != nil {
		s.log.Error("insert runner token", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// Consume the single-use token row (Phase 7). Atomic within this tx: if a
	// racing registration consumed it between checkRegistrationToken and here,
	// the guard matches 0 rows and THIS registration aborts — the runners/
	// runner_tokens inserts above roll back with it.
	if chk.RowID != 0 {
		won, err := consumeRegistrationToken(r.Context(), tx, chk.RowID, runnerID, ts)
		if err != nil {
			s.log.Error("consume registration token", "id", chk.RowID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if !won {
			httpx.Fail(w, http.StatusUnauthorized, "token_used",
				"registration token was consumed or revoked meanwhile — tokens are single-use; mint a new one")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db commit error")
		return
	}

	var caps []string
	_ = json.Unmarshal(capsJSON, &caps)
	var respToolchains json.RawMessage
	if toolchainsVal != nil {
		respToolchains = json.RawMessage(toolchainsVal.(string))
	}
	resp := runnerResponse{
		ID:                  runnerID,
		Name:                req.Name,
		Status:              "online",
		OS:                  req.OS,
		Capabilities:        caps,
		Load:                0,
		MaxConcurrent:       req.MaxConcurrent,
		Version:             req.Version,
		Inventory:           req.Inventory,
		ProtocolVersion:     req.ProtocolVersion,
		RegisteredAt:        ts,
		RegistrationTokenID: regTokID,
		Toolchains:          respToolchains,
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"runner": resp,
		"apiKey": apiKey,
	})
}

// HandleDeregisterRunner revokes a runner's registration (operator, CSRF required).
// DELETE /api/v1/runners/{id}
func (s *Service) HandleDeregisterRunner(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// Verify the runner exists.
	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// Revoke all runner_tokens associated with this runner by name pattern.
	// This approach is defensive: we revoke tokens whose created_by matches
	// the runner name pattern set at registration.
	ts := now()
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE runner_tokens SET revoked_at = ?
		WHERE created_by = ? AND revoked_at IS NULL`,
		ts, "runner:"+name); err != nil {
		s.log.Error("revoke runner tokens", "error", err)
	}

	// DR-7: snapshot the operator-owned placement BEFORE the delete — the
	// runner_agencies rows cascade away with the runner row, so afterwards there
	// is nothing left to record. A failure here aborts the whole deregistration
	// rather than proceeding: losing placement silently is the bug this exists to
	// prevent, and the operator can retry.
	if err := capturePlacement(r.Context(), tx, runnerID, name, sessionActor(r), deregisterViaOperator, ts); err != nil {
		s.log.Error("capture runner placement", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	// DRF-4: the feed must be able to answer "who removed runner X?" — the same
	// AA-4 rule drain/keyscan/resync already follow. In the transaction, so a
	// deregistration without its audit row cannot exist.
	if err := auditlog.WriteActivity(r.Context(), tx, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      sessionActor(r),
		Target:     "runner:" + name,
		RunnerName: name,
		Summary:    "deregistered",
	}); err != nil {
		s.log.Error("record runner deregistration", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// Delete the runner row.
	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM runners WHERE id = ?`, runnerID); err != nil {
		s.log.Error("delete runner", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	if err := tx.Commit(); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db commit error")
		return
	}

	// Drop the drift flap-guard entry — ids are never reused, so keeping it
	// would only leak an entry per deregistered runner for the process lifetime.
	s.driftMu.Lock()
	delete(s.driftLastOp, runnerID)
	s.driftMu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// bearerToken extracts the Bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

// HandleTestRunner runs a connection test for a runner. Since runner agents
// poll the server, we verify connection by checking if the runner has checked
// in recently (is online/draining).
func (s *Service) HandleTestRunner(w http.ResponseWriter, r *http.Request) {
	idn, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}

	runnerID := r.PathValue("id")

	// Fetch current status, last_seen_at, version, os from the DB
	var name, status, os, version string
	var lastSeenAt sql.NullString
	err := s.db.QueryRowContext(r.Context(), `
		SELECT name, status, os, version, last_seen_at
		FROM runners WHERE id = ?`, runnerID).
		Scan(&name, &status, &os, &version, &lastSeenAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		} else {
			s.log.Error("test runner db query", "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		}
		return
	}

	checkedAt := now()

	isOnline := false
	var lastSeenTime time.Time
	if lastSeenAt.Valid && lastSeenAt.String != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, lastSeenAt.String); parseErr == nil {
			lastSeenTime = parsed
			if (status == "online" || status == "draining") && time.Since(parsed) < s.cfg.RunnerOfflineAfter {
				isOnline = true
			}
		}
	}

	var resultStatus string
	var message string
	var latencyMs int64

	if isOnline {
		resultStatus = "verified"
		timeAgo := time.Since(lastSeenTime).Round(time.Second)
		message = fmt.Sprintf("Runner '%s' is online. Last heartbeat: %v ago. OS: %s, Version: %s", name, timeAgo, os, version)
		latencyMs = timeAgo.Milliseconds()
	} else {
		resultStatus = "conn_error"
		if !lastSeenAt.Valid || lastSeenAt.String == "" {
			message = fmt.Sprintf("Runner '%s' has never connected to the server.", name)
		} else {
			timeAgo := time.Since(lastSeenTime).Round(time.Second)
			message = fmt.Sprintf("Runner '%s' is offline. Last seen: %v ago. OS: %s, Version: %s", name, timeAgo, os, version)
		}
	}

	_ = settings.WriteChangeLog(r.Context(), s.db, idn.Email, "Runners", "tested", name, resultStatus)

	httpx.JSON(w, http.StatusOK, map[string]any{
		"status":    resultStatus,
		"message":   message,
		"latencyMs": latencyMs,
		"checkedAt": checkedAt,
	})
}
