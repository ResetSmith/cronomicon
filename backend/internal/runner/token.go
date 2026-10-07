package runner

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// hashToken is a local alias for auth.HashToken so this file doesn't need to
// reference it indirectly everywhere.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// registrationTokenInfo is the list-item shape for registration tokens
// (runner-install-update.md Phase 7, D6). NEVER carries plaintext — that is
// returned exactly once by the mint response.
type registrationTokenInfo struct {
	ID        int64   `json:"id"`
	Label     *string `json:"label,omitempty"` // operator-chosen, e.g. the intended runner name
	CreatedBy string  `json:"createdBy"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt string  `json:"expiresAt"`
	RevokedAt *string `json:"revokedAt,omitempty"`
	// UsedAt/UsedByRunner* are the audit trail: which runner consumed this
	// token, set atomically by its first (and only) successful registration.
	UsedAt           *string `json:"usedAt,omitempty"`
	UsedByRunnerID   *string `json:"usedByRunnerId,omitempty"`
	UsedByRunnerName *string `json:"usedByRunnerName,omitempty"` // resolved at list time; absent if the runner was since deregistered
	// AgencyID is the agency that will OWN the agent this token enrols (LR-32,
	// LR-61): the token is the operator's grant of a placement, held here and
	// never declared by the agent. `global` for a Global-owned agent. AgencyName
	// is resolved at list time and empty if the agency has since been deleted —
	// in which case the token no longer enrols anything (LR-33).
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	// Status is derived for display: active | revoked | expired | pending.
	// Precedence active > revoked > expired — a consumed token stays "active"
	// forever; that's the interesting audit fact.
	Status string `json:"status"`
}

// mintedTokenResponse is the POST (mint) response: the info row plus the
// plaintext, shown once and never re-served.
type mintedTokenResponse struct {
	registrationTokenInfo
	Token string `json:"token"`
}

// tokenStatus derives the display status with active > revoked > expired > pending.
func tokenStatus(usedAt, revokedAt *string, expiresAt, nowTS string) string {
	switch {
	case usedAt != nil && *usedAt != "":
		return "active"
	case revokedAt != nil && *revokedAt != "":
		return "revoked"
	case expiresAt <= nowTS:
		return "expired"
	default:
		return "pending"
	}
}

// MintRequest is the optional JSON body for POST /runners/registration-tokens.
// AgencyID is the owner of the agent the token will enrol; the API layer has
// resolved and authorized it before this handler sees it (an omitted one is
// Global there only for a global administrator). Empty here is Global.
//
// Exported so the API layer decodes the caller's body into THIS type and hands
// the service a re-encoding of it: one reading of the body, so the agency that
// was authorized is the agency that is stored.
type MintRequest struct {
	Label    string `json:"label"`
	AgencyID string `json:"agencyId"`
}

// HandleMintRegistrationToken mints a single-use registration token (Phase 7).
// Operator (ConfigureApp), CSRF required. Unlike the retired shared-token
// rotate, minting does NOT revoke other tokens — several installs can be in
// flight, each with its own token. Plaintext is returned once; hash-at-rest.
// POST /api/v1/runners/registration-tokens
func (s *Service) HandleMintRegistrationToken(w http.ResponseWriter, r *http.Request) {
	// Attribution only — the route is behind requirePerm(ConfigureApp).
	createdBy := sessionActor(r)

	// Body is optional (absent/empty ⇒ unlabeled), but a PRESENT-and-malformed
	// body is rejected — silently dropping a mistyped label would mint a
	// working token the operator can't tell apart in the list.
	var req MintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"request body must be valid JSON (optional: {\"label\": \"...\"})")
		return
	}
	if len(req.Label) > 120 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"label must be 120 characters or fewer")
		return
	}

	agencyID := strings.TrimSpace(req.AgencyID)
	if agencyID == "" {
		agencyID = agencyid.Global
	}
	var agencyName string
	if err := s.db.QueryRowContext(r.Context(), `SELECT name FROM agencies WHERE id = ?`, agencyID).Scan(&agencyName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "agency "+agencyID+" does not exist")
			return
		}
		s.log.Error("mint registration token: read agency", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	token, err := generateToken(tokenPrefix)
	if err != nil {
		s.log.Error("generate registration token", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "failed to generate token")
		return
	}

	ts := now()
	expiresAt := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	var labelVal any // NULL when empty
	if req.Label != "" {
		labelVal = req.Label
	}

	res, err := s.db.ExecContext(r.Context(), `
		INSERT INTO registration_tokens(token_hash, created_by, created_at, expires_at, label, agency_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(token), createdBy, ts, expiresAt, labelVal, agencyID)
	if err != nil {
		s.log.Error("insert registration token", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	rowID, _ := res.LastInsertId()

	// Audit entry (parity with drain/resync).
	// The audit row says whose agent the token enrols: minting one is granting a
	// placement.
	summary := "registration token minted for an agent owned by " + agencyName
	if req.Label != "" {
		summary += " (" + req.Label + ")"
	}
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		At:      ts,
		Kind:    "config",
		Actor:   createdBy,
		Target:  "runners",
		Summary: summary,
	})

	info := registrationTokenInfo{
		ID:         rowID,
		CreatedBy:  createdBy,
		CreatedAt:  ts,
		ExpiresAt:  expiresAt,
		AgencyID:   agencyID,
		AgencyName: agencyName,
		// What the list will call it a moment from now: unused, so pending. (It
		// said "active" here, the list's word for a token that HAS been used.)
		Status: tokenStatus(nil, nil, expiresAt, ts),
	}
	if req.Label != "" {
		info.Label = &req.Label
	}
	httpx.JSON(w, http.StatusCreated, mintedTokenResponse{registrationTokenInfo: info, Token: token})
}

// RegistrationTokens lists registration tokens — label, owner agency, created,
// expires, revoked, and the used-by audit trail. Never plaintext (only hashes
// are stored). Newest first, capped at 100 rows.
//
// It returns EVERY token. Who may see which is the caller's to decide, by the
// token's agency (LR-36): the list was open to any session until 2.3.0, and a
// token's label and creator are not everyone's to read once agencies enrol
// their own agents. The route is the API layer's (api.handleListRegistrationTokens).
func (s *Service) RegistrationTokens(ctx context.Context) ([]registrationTokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rt.id, rt.label, rt.created_by, rt.created_at, rt.expires_at,
		       rt.revoked_at, rt.used_at, rt.used_by_runner_id, ru.name,
		       rt.agency_id, COALESCE(ag.name, '')
		FROM registration_tokens rt
		LEFT JOIN runners ru ON ru.id = rt.used_by_runner_id
		LEFT JOIN agencies ag ON ag.id = rt.agency_id
		ORDER BY rt.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nowTS := now()
	out := []registrationTokenInfo{}
	for rows.Next() {
		var t registrationTokenInfo
		if err := rows.Scan(&t.ID, &t.Label, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt,
			&t.RevokedAt, &t.UsedAt, &t.UsedByRunnerID, &t.UsedByRunnerName,
			&t.AgencyID, &t.AgencyName); err != nil {
			return nil, err
		}
		t.Status = tokenStatus(t.UsedAt, t.RevokedAt, t.ExpiresAt, nowTS)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RegistrationTokenAgency returns the agency a token will enrol an agent for,
// so the API layer can authorize a revoke by it. found is false for an id that
// matches nothing.
func (s *Service) RegistrationTokenAgency(ctx context.Context, rowID int64) (agencyID string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT agency_id FROM registration_tokens WHERE id = ?`, rowID).Scan(&agencyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return agencyID, err == nil, err
}

// HandleRevokeRegistrationToken revokes an UNUSED registration token (Phase 7
// — this replaces the shared token's "Regenerate": there is nothing to rotate
// anymore, you just kill a token that shouldn't be usable). Operator
// (ConfigureApp), CSRF required. A used token cannot be revoked (409) — it is
// already dead as a credential and its row is the audit record; revoking the
// runner it registered is Deregister's job. Revoking an already-revoked token
// is a no-op 204.
// DELETE /api/v1/runners/registration-tokens/{id}
func (s *Service) HandleRevokeRegistrationToken(w http.ResponseWriter, r *http.Request) {
	rowID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "registration token not found")
		return
	}

	var usedAt, revokedAt *string
	var label *string
	err = s.db.QueryRowContext(r.Context(), `
		SELECT used_at, revoked_at, label FROM registration_tokens WHERE id = ?`, rowID).
		Scan(&usedAt, &revokedAt, &label)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "registration token not found")
		return
	}
	if usedAt != nil && *usedAt != "" {
		httpx.Fail(w, http.StatusConflict, "token_used",
			"token was already used to register a runner — to revoke that runner, deregister it")
		return
	}
	if revokedAt == nil || *revokedAt == "" {
		ts := now()
		res, err := s.db.ExecContext(r.Context(), `
			UPDATE registration_tokens SET revoked_at = ? WHERE id = ? AND used_at IS NULL`,
			ts, rowID)
		if err != nil {
			s.log.Error("revoke registration token", "id", rowID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		// TOCTOU guard: a registration may have consumed the token between the
		// read above and this UPDATE (its used_at IS NULL guard then matches
		// nothing). Report the truth — 409, not a false "revoked" 204.
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.Fail(w, http.StatusConflict, "token_used",
				"token was already used to register a runner — to revoke that runner, deregister it")
			return
		}
		summary := fmt.Sprintf("registration token #%d revoked", rowID)
		if label != nil && *label != "" {
			summary = fmt.Sprintf("registration token #%d (%s) revoked", rowID, *label)
		}
		actor := sessionActor(r)
		_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
			At:      ts,
			Kind:    "config",
			Actor:   actor,
			Target:  "runners",
			Summary: summary,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// regTokenCheck is the outcome of checking a presented registration token.
type regTokenCheck struct {
	OK bool
	// RowID is the matched registration_tokens row (0 for the env bootstrap
	// token, which has no row and is deliberately multi-use — Phase 7 leaves
	// CRONOMICON_RUNNER_BOOTSTRAP_TOKEN unchanged).
	RowID int64
	// AgencyID is the agency that will own the agent this token enrols (LR-61):
	// the row's, or Global for the bootstrap token. Set only when OK.
	AgencyID string
	// Code/Msg describe the failure when !OK. Codes are deliberately distinct
	// (Phase 7): "token_used" (with the consuming runner's name) tells the
	// operator a token was double-used; "token_expired" says mint a new one;
	// plain "unauthorized" covers unknown/revoked.
	Code string
	Msg  string
}

// checkRegistrationToken validates a presented first-contact registration
// token WITHOUT consuming it — consumption happens atomically inside the
// registration transaction (consumeRegistrationToken), so two racing
// registrations can both pass this check but only one can win the row.
func (s *Service) checkRegistrationToken(ctx context.Context, token string) (regTokenCheck, error) {
	var (
		rowID      int64
		expiresAt  string
		revokedAt  *string
		usedAt     *string
		usedByName *string
	)
	var agencyID string
	var agencyExists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT rt.id, rt.expires_at, rt.revoked_at, rt.used_at, ru.name,
		       rt.agency_id, EXISTS (SELECT 1 FROM agencies ag WHERE ag.id = rt.agency_id)
		FROM registration_tokens rt
		LEFT JOIN runners ru ON ru.id = rt.used_by_runner_id
		WHERE rt.token_hash = ?
		ORDER BY rt.id DESC LIMIT 1`,
		hashToken(token)).Scan(&rowID, &expiresAt, &revokedAt, &usedAt, &usedByName, &agencyID, &agencyExists)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not a DB token — fall through to the bootstrap comparison below.
	case err != nil:
		return regTokenCheck{}, err
	case usedAt != nil && *usedAt != "":
		msg := "registration token already used"
		if usedByName != nil && *usedByName != "" {
			msg = "registration token already used by runner " + *usedByName
		}
		return regTokenCheck{Code: "token_used", Msg: msg + " — tokens are single-use; mint a new one"}, nil
	case revokedAt != nil && *revokedAt != "":
		return regTokenCheck{Code: "unauthorized", Msg: "registration token revoked"}, nil
	case expiresAt <= now():
		return regTokenCheck{Code: "token_expired", Msg: "registration token expired (24h) — mint a new one"}, nil
	case !agencyExists:
		// LR-33: the agency this token was minted for is gone. There is no
		// fallback into Global, which would put a department's machine on shared
		// work; the token enrols nothing and is not consumed.
		return regTokenCheck{Code: "agency_gone", Msg: "the agency this registration token was minted for no longer exists — mint a new one"}, nil
	default:
		return regTokenCheck{OK: true, RowID: rowID, AgencyID: agencyID}, nil
	}

	// Bootstrap token from config (A6.1 seed) — constant-time comparison to
	// prevent timing-based token enumeration. Multi-use by design; no row.
	if s.cfg.RunnerBootstrapToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.RunnerBootstrapToken)) == 1 {
		// The environment bootstrap token has no row, so it cannot name an
		// agency: it enrols a Global-owned agent serving Global, as a global
		// administrator's token for Global does (LR-61).
		return regTokenCheck{OK: true, AgencyID: agencyid.Global}, nil
	}
	return regTokenCheck{Code: "unauthorized", Msg: "registration token invalid or expired"}, nil
}

// consumeRegistrationToken marks a token row used by runnerID inside the
// registration transaction. The `used_at IS NULL` guard makes consumption
// atomic: of two racing registrations presenting the same token, exactly one
// UPDATE matches — the loser aborts its registration (Phase 7's
// two-runners-one-token race guard). Returns false when the row was already
// consumed (or revoked meanwhile).
func consumeRegistrationToken(ctx context.Context, tx *sql.Tx, rowID int64, runnerID, ts string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE registration_tokens
		SET used_at = ?, used_by_runner_id = ?
		WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		ts, runnerID, rowID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
