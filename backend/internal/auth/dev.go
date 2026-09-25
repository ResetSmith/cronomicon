package auth

import (
	"net/http"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// devIdentity is the synthetic operator minted by the dev-login bypass. It is an
// admin holding the explicit AllScopes ("*") grant, so Identity.Unrestricted() is
// true and every scope's runs/secrets are visible — the whole UI can be previewed.
// (Under the A5 fix "unrestricted" is the "*" grant, not an empty set; the guards now
// read "*", so the dev bypass must carry it. See the scoping-fix plan §5.)
func devIdentity() Identity {
	return Identity{
		Email:         "developer@cronomicon.local",
		DisplayName:   "Developer (bypass)",
		Groups:        []string{"cronomicon-admins"},
		Roles:         []string{"admin"},
		AllowedScopes: []string{AllScopes}, // "*" ⇒ explicitly unrestricted (sees all)
		// RB-14: synthesize the equivalent unrestricted grant. The dev identity is
		// constructed rather than resolved, so it never passes through ResolveGrants
		// — without this it would carry no grants and read as ZERO access the moment
		// RB-15 makes the field authoritative, breaking `make dev` in the release
		// that can least afford a confusing failure.
		Grants:   []RoleGrant{{Role: "admin", Scopes: []string{AllScopes}}},
		IssuedAt: time.Now().UTC(),
		LastSeen: time.Now().UTC(),
	}
}

// DevEnabled reports whether the local dev-login bypass is active (CRONOMICON_DEV_AUTH).
func (s *Service) DevEnabled() bool { return s.devAuth }

// DevLogin establishes an operator session for the synthetic Developer identity
// WITHOUT going through the identity provider. It only works when CRONOMICON_DEV_AUTH=true; the
// route is not even mounted otherwise, but we re-check here as defence in depth.
func (s *Service) DevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.devAuth {
		httpx.Fail(w, http.StatusNotFound, "not_found", "dev login is not enabled")
		return
	}
	id := devIdentity()
	id.Epoch = s.currentSessionEpoch() // SU-5: stamp so the epoch check accepts this session
	if err := s.recordLogin(r.Context(), id); err != nil {
		s.log.Error("dev recording login failed", "error", err)
		// non-fatal: the session still establishes
	}
	if err := s.codec.write(w, id); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "session_failed", "could not establish session")
		return
	}
	// Audit loudly: this mints admin with "*" scopes and no identity provider in
	// the loop. If it is ever enabled somewhere it should not be, this row is the
	// evidence.
	s.auditAuth(r.Context(), r, auditlog.AuthEventParams{
		Kind: auditlog.AuthDevAuth, Outcome: auditlog.OutcomeSuccess, Actor: id.Email,
		Reason:  "dev_auth_bypass",
		Details: "synthetic admin session with scopes [*]; no identity provider consulted",
	})
	s.issueCSRF(w)
	s.log.Warn("dev-login: synthetic admin session established", "email", id.Email)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Providers reports the active auth mode and which login methods this server
// offers, so the SPA can render the right login experience (A.9). Public: it
// leaks no secrets, and the login page needs it pre-session. In trusted-header
// mode the in-app OIDC button is dead (the proxy authenticates), so the SPA uses
// `mode` to suppress it; `logoutUrl` is where sign-out should navigate.
func (s *Service) Providers(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"mode":      s.mode,
		"oidc":      s.enabled,
		"dev":       s.devAuth,
		"logoutUrl": s.logoutRedirectURL,
	})
}
