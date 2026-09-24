package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// CSRF double-submit (T8): a non-httpOnly cookie holds a random token the SPA
// reads and echoes in the X-CSRF-Token header on every state-changing request.
// The server requires the header to match the cookie. Because an attacker on
// another origin can't read the cookie (SameSite=Lax + same-origin JS), they
// can't forge the header.
const (
	csrfCookieName = "amadeus_csrf"
	csrfHeaderName = "X-CSRF-Token"
)

// issueCSRF sets a fresh CSRF cookie. Called at login; the SPA also gets it
// refreshed on any authenticated GET that lacks one.
func (s *Service) issueCSRF(w http.ResponseWriter) string {
	token := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // readable by the SPA so it can echo it in the header
		Secure:   s.codec.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return token
}

// RequireCSRF rejects state-changing requests whose X-CSRF-Token header does not
// match the CSRF cookie. Safe methods pass through. Apply to operator
// state-changing routes (T8); never to runner routes (T9 — bearer, no cookies).
func (s *Service) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(csrfCookieName)
		header := r.Header.Get(csrfHeaderName)
		if err != nil || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
			// RequireCSRF runs INSIDE RequireSession, so the actor is known — a CSRF
			// failure is attributable to a named user, which is what makes it worth
			// auditing rather than just rejecting.
			actor := ""
			if id, ok := IdentityFrom(r.Context()); ok {
				actor = id.Email
			}
			s.auditAuth(r.Context(), r, auditlog.AuthEventParams{
				Kind: auditlog.AuthCSRFFailed, Outcome: auditlog.OutcomeFailure,
				Actor: actor, Reason: "csrf_token_mismatch", Target: r.URL.Path,
			})
			httpx.Fail(w, http.StatusForbidden, "csrf_failed", "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// touchCSRF ensures an authenticated GET response carries a CSRF cookie so a
// freshly-loaded SPA always has a token to submit.
func (s *Service) touchCSRF(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(csrfCookieName); err != nil {
		s.issueCSRF(w)
	}
}
