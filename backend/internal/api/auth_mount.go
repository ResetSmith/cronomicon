package api

import "net/http"

// mountAuth registers the operator identity surface (B2):
//
//	GET  /api/v1/auth/login            → redirect to the identity provider (public)
//	GET  /api/v1/auth/callback         → OIDC callback (public)
//	POST /api/v1/auth/logout           → clear session (session + CSRF)
//	GET  /api/v1/me                    → current operator (session)
//	GET  /api/v1/access/recent-logins  → Honest View, A3.2 (session)
//
// Login/callback are public (they establish the session). Logout is a
// state-changing operator action, so it sits behind session + CSRF (T8).
func (s *Server) mountAuth(mux *http.ServeMux) {
	if s.auth == nil {
		return
	}
	a := s.auth

	// OIDC relying-party endpoints exist only in oidc mode (the optional path).
	// In trusted-header mode the reverse proxy authenticates, so /login and
	// /callback are never mounted.
	if a.OIDCMode() {
		mux.HandleFunc("GET /api/v1/auth/login", a.Login)
		mux.HandleFunc("GET /api/v1/auth/callback", a.Callback)
	}
	mux.Handle("POST /api/v1/auth/logout", a.RequireSession(a.RequireCSRF(http.HandlerFunc(a.Logout))))

	// Public login-method discovery so the SPA renders the right buttons (SSO
	// and/or the dev bypass) before any session exists.
	mux.HandleFunc("GET /api/v1/auth/providers", a.Providers)

	// Dev login bypass (AMADEUS_DEV_AUTH) — local preview only. Only mounted when
	// enabled so production never exposes the route at all.
	if a.DevEnabled() {
		mux.HandleFunc("GET /api/v1/auth/dev-login", a.DevLogin)
	}

	mux.Handle("GET /api/v1/me", a.RequireSession(http.HandlerFunc(a.Me)))
	// Spec path is /recent-logins (openapi.yaml); /access/recent-logins kept as a
	// back-compat alias.
	mux.Handle("GET /api/v1/recent-logins", a.RequireSession(http.HandlerFunc(a.RecentLogins)))
	mux.Handle("GET /api/v1/access/recent-logins", a.RequireSession(http.HandlerFunc(a.RecentLogins)))
}
