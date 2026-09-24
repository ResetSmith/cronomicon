package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

const (
	stateCookie = "amadeus_oidc_state"
	nonceCookie = "amadeus_oidc_nonce"
	loginTTL    = 10 * time.Minute
)

// claims is the subset of the ID token Cronomicon consumes. `groups` is the AD
// group claim the identity provider surfaces (A3.1), the basis for role derivation.
type claims struct {
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
	Nonce  string   `json:"nonce"`
}

// Login starts the OIDC auth-code flow: stash state+nonce in short-lived cookies
// and redirect to the identity provider.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	if !s.enabled {
		httpx.Fail(w, http.StatusServiceUnavailable, "oidc_unavailable", "identity provider not configured")
		return
	}
	state := base64.RawURLEncoding.EncodeToString(randomBytes(24))
	nonce := base64.RawURLEncoding.EncodeToString(randomBytes(24))
	s.setLoginCookie(w, stateCookie, state)
	s.setLoginCookie(w, nonceCookie, nonce)
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oidc.Nonce(nonce)), http.StatusFound)
}

// Callback completes the flow: validate state, exchange the code, verify the ID
// token + nonce, derive roles (A3.1) and allowed scopes (A5), record the login
// (A3.2 Honest View), and establish the operator session (T8).
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	if !s.enabled {
		httpx.Fail(w, http.StatusServiceUnavailable, "oidc_unavailable", "identity provider not configured")
		return
	}
	ctx := r.Context()

	stateCk, err := r.Cookie(stateCookie)
	if err != nil || r.URL.Query().Get("state") != stateCk.Value {
		s.auditLoginFailed(ctx, r, "bad_state")
		httpx.Fail(w, http.StatusBadRequest, "bad_state", "invalid OAuth state")
		return
	}
	// SU-7: route the back-channel token exchange (POST to the IdP token endpoint)
	// through the same SSRF egress-guarded client used for discovery/JWKS. oauth2
	// picks the client up from the context via the oauth2.HTTPClient key.
	if s.oidcHTTPClient != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, s.oidcHTTPClient)
	}
	oauth2Token, err := s.oauth.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		s.auditLoginFailed(ctx, r, "exchange_failed")
		httpx.Fail(w, http.StatusBadGateway, "exchange_failed", "token exchange failed")
		return
	}
	rawID, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		s.auditLoginFailed(ctx, r, "no_id_token")
		httpx.Fail(w, http.StatusBadGateway, "no_id_token", "no id_token in token response")
		return
	}
	idToken, err := s.verifier.Verify(ctx, rawID)
	if err != nil {
		s.auditLoginFailed(ctx, r, "id_token_invalid")
		httpx.Fail(w, http.StatusUnauthorized, "id_token_invalid", "id_token verification failed")
		return
	}
	var cl claims
	if err := idToken.Claims(&cl); err != nil {
		s.auditLoginFailed(ctx, r, "claims_failed")
		httpx.Fail(w, http.StatusBadGateway, "claims_failed", "could not parse id_token claims")
		return
	}
	nonceCk, err := r.Cookie(nonceCookie)
	if err != nil || cl.Nonce != nonceCk.Value {
		s.auditLoginFailed(ctx, r, "bad_nonce")
		httpx.Fail(w, http.StatusBadRequest, "bad_nonce", "invalid OIDC nonce")
		return
	}

	// RB-15: grants are AUTHORITATIVE, and Roles/AllowedScopes are DERIVED from them
	// rather than resolved on independent axes — that independence is what produced
	// the cross-product leak. A failure fails the login: continuing would mint an
	// identity with no grants, which after the switch means zero authority.
	grants, err := ResolveGrants(ctx, s.db, cl.Groups)
	if err != nil {
		s.log.Error("grant resolution failed", "error", err)
		s.auditLoginFailed(ctx, r, "grant_resolution_failed")
		httpx.Fail(w, http.StatusInternalServerError, "grant_resolution_failed", "could not resolve access grants")
		return
	}
	roles := UnionGrantRoles(grants)
	// Visibility stays unioned across grants (§2.2) — see the header path.
	scopes := UnionGrantScopes(grants)

	id := Identity{
		Email:         cl.Email,
		DisplayName:   cl.Name,
		Groups:        cl.Groups,
		Roles:         roles,
		AllowedScopes: scopes,
		Grants:        grants,
		IssuedAt:      time.Now().UTC(),
		LastSeen:      time.Now().UTC(),        // FX-E4: the idle clock starts at login
		Epoch:         s.currentSessionEpoch(), // SU-5: stamp so RBAC changes revoke this session
	}
	if err := s.recordLogin(ctx, id); err != nil {
		s.log.Error("recording login failed", "error", err, "email", id.Email)
		// non-fatal: the user is authenticated even if the Honest-View row fails
	}
	if err := s.codec.write(w, id); err != nil {
		s.auditLoginFailedAs(ctx, r, id.Email, "session_failed")
		httpx.Fail(w, http.StatusInternalServerError, "session_failed", "could not establish session")
		return
	}
	s.auditAuth(ctx, r, auditlog.AuthEventParams{
		Kind: auditlog.AuthLogin, Outcome: auditlog.OutcomeSuccess, Actor: id.Email,
		Details: "oidc",
	})
	s.issueCSRF(w)
	s.clearLoginCookies(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Logout clears the session and CSRF cookies. In trusted-header mode the
// "session" is the SSO cookie validated by the proxy, which the app can't
// clear; instead it returns the configured identity-provider logout URL so the SPA can
// navigate there (the request is an XHR, so a 302 wouldn't redirect the browser).
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	// Logout is mounted behind RequireSession, so the identity is in hand — this
	// is one of the few auth events that can always name its actor.
	actor := ""
	if id, ok := IdentityFrom(r.Context()); ok {
		actor = id.Email
	}
	s.auditAuth(r.Context(), r, auditlog.AuthEventParams{
		Kind: auditlog.AuthLogout, Outcome: auditlog.OutcomeSuccess, Actor: actor,
	})
	s.codec.clear(w)
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "", Path: "/", MaxAge: -1})
	if s.logoutRedirectURL != "" {
		httpx.JSON(w, http.StatusOK, map[string]string{"redirect": s.logoutRedirectURL})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the authenticated operator (openapi GET /me). Requires a session.
func (s *Service) Me(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	httpx.JSON(w, http.StatusOK, id)
}

// RecentLogins returns the Honest View (A3.2): users who have actually logged in.
func (s *Service) RecentLogins(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT email, display_name, groups, first_seen_at, last_login_at
		FROM recent_logins ORDER BY last_login_at DESC`)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "query_failed", "could not list recent logins")
		return
	}
	defer rows.Close()

	// loginRow's JSON tags must match the RecentLogin schema in openapi.yaml
	// (email/name/adGroups/grants/firstSeenAt/lastSeenAt) so the SPA can render the
	// "Last seen" column — mismatched tags were bug LB4.
	type loginRow struct {
		Email       string      `json:"email"`
		DisplayName string      `json:"name"`
		Groups      []string    `json:"adGroups"`
		Grants      []grantView `json:"grants"`
		FirstSeenAt string      `json:"firstSeenAt"`
		LastLoginAt string      `json:"lastSeenAt"`
	}
	out := []loginRow{}
	for rows.Next() {
		var lr loginRow
		var groupsJSON string
		if err := rows.Scan(&lr.Email, &lr.DisplayName, &groupsJSON, &lr.FirstSeenAt, &lr.LastLoginAt); err != nil {
			httpx.Fail(w, http.StatusInternalServerError, "scan_failed", "could not read recent logins")
			return
		}
		_ = json.Unmarshal([]byte(groupsJSON), &lr.Groups)
		out = append(out, lr)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "scan_failed", "could not read recent logins")
		return
	}
	rows.Close() // close BEFORE resolving — a query issued inside an open cursor
	// contends for the pool (db.maxOpenConns); ResolveGrants issues two of its own.

	// RB-21 — the Honest View reports each user's GRANTS, not one role name.
	//
	// It carried a single `resolvedRole` until v0.57.8, and under multi-grant that
	// name was actively false rather than merely incomplete (RB-Q6). "Alice:
	// operator" is what an auditor reads as "Alice can operate", when the truth is
	// "Alice can operate Finance, and can only look at Tax". Collapsing a set of
	// (role, where) pairs to the highest role discards the half that bounds it —
	// on the one screen whose entire purpose is answering who holds what.
	//
	// Agency NAMES are resolved once, up front, rather than per row: the ids on a
	// grant are meaningless to a reader, and a per-row lookup would put a query
	// inside this loop for a table that is two dozen rows at most.
	agencyNames, err := agencyNameByID(r.Context(), s.db)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "query_failed", "could not read agencies")
		return
	}
	for i := range out {
		grants, err := ResolveGrants(r.Context(), s.db, out[i].Groups)
		if err != nil {
			httpx.Fail(w, http.StatusInternalServerError, "role_resolution_failed", "could not resolve roles")
			return
		}
		out[i].Grants = grantViews(grants, agencyNames)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out})
}

// recordLogin upserts the Honest-View row (A3.2): first_seen_at is preserved,
// last_login_at + groups + display_name refreshed on every login.
func (s *Service) recordLogin(ctx context.Context, id Identity) error {
	now := time.Now().UTC().Format(time.RFC3339)
	groupsJSON, _ := json.Marshal(id.Groups)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			display_name = excluded.display_name,
			groups       = excluded.groups,
			last_login_at = excluded.last_login_at`,
		id.Email, id.DisplayName, string(groupsJSON), now, now)
	return err
}

func (s *Service) setLoginCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		HttpOnly: true, Secure: s.codec.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(loginTTL.Seconds()),
	})
}

func (s *Service) clearLoginCookies(w http.ResponseWriter) {
	for _, n := range []string{stateCookie, nonceCookie} {
		http.SetCookie(w, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1})
	}
}

// auditLoginFailed records an OIDC login failure with no known identity — the
// common case, since most of the callback's exits happen before any claim is
// verified. A NULL actor is legal in auth_events precisely for these.
func (s *Service) auditLoginFailed(ctx context.Context, r *http.Request, reason string) {
	s.auditLoginFailedAs(ctx, r, "", reason)
}

// auditLoginFailedAs is auditLoginFailed for the later exits, where the identity
// has been established and the failure is ours rather than the caller's.
func (s *Service) auditLoginFailedAs(ctx context.Context, r *http.Request, actor, reason string) {
	s.auditAuth(ctx, r, auditlog.AuthEventParams{
		Kind: auditlog.AuthLoginFailed, Outcome: auditlog.OutcomeFailure,
		Actor: actor, Reason: reason, Details: "oidc",
	})
}

// grantView is one (role, where) pair as the Honest View renders it: what this
// person may do, and where. It is the display shape of RoleGrant, with the agency
// id resolved to a name and the expanded scope list dropped — a reader needs
// "Finance", not the eleven scopes Finance currently contains.
type grantView struct {
	Role string `json:"role"`
	// AgencyName is the department the grant was authored against. Empty exactly
	// when AllScopes is true.
	AgencyName string `json:"agencyName,omitempty"`
	// AllScopes marks the unrestricted grant. It is a separate flag rather than a
	// magic AgencyName ("*", "All scopes") because the UI must be able to style it
	// differently — an unrestricted grant is the one worth auditing first, and a
	// sentinel string sorts in among real department names.
	AllScopes bool `json:"allScopes"`
}

// agencyNameByID loads the agency id → name map used to render grants.
func agencyNameByID(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM agencies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// grantViews renders a resolved grant set for display. ResolveGrants has already
// deduped and sorted (role, then agency), so the order here is stable across
// requests — a list that reshuffles between refreshes is unreadable in a table.
//
// A grant whose agency has been deleted out from under it renders with the raw id
// rather than blank: an unresolvable grant is a data problem worth seeing, and a
// blank "where" is indistinguishable from unrestricted, which is the most
// dangerous thing it could be mistaken for.
func grantViews(grants []RoleGrant, agencyNames map[string]string) []grantView {
	out := make([]grantView, 0, len(grants))
	for _, g := range grants {
		v := grantView{Role: g.Role}
		if g.Unrestricted() {
			v.AllScopes = true
		} else if name, ok := agencyNames[g.Agency]; ok {
			v.AgencyName = name
		} else {
			v.AgencyName = g.Agency
		}
		out = append(out, v)
	}
	return out
}
