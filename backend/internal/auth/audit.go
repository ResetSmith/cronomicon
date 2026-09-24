package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// Auth auditing (LU-9).
//
// Before this, ZERO authentication or authorization events were recorded
// anywhere: no login success or failure, no logout, no RBAC denial, no CSRF
// rejection — to either audit table. `recent_logins` is not a substitute and its
// own documentation says so: it is an UPSERT keyed on the user, so login #2
// destroys the record of login #1.
//
// Two things make this harder than "log the OIDC callback":
//
//  1. trusted-header is the DEFAULT mode. There is no callback and no login
//     event in it at all — identity is minted per REQUEST in identityFromHeaders.
//     An implementation written only against the OIDC path would audit nothing in
//     the deployment that actually ships, and one written naively against the
//     header path would write a row per HTTP request. Hence the throttle.
//  2. The client address is not simply r.RemoteAddr. See httpx.ClientIP.

// auditAuth records one auth event, enriched with the request's addressing.
//
// Failures are logged and swallowed. This runs on the login path, and a full
// disk or a locked database must never be the reason someone cannot sign in —
// the same discipline recordLogin already follows.
func (s *Service) auditAuth(ctx context.Context, r *http.Request, p auditlog.AuthEventParams) {
	if r != nil {
		// Both addresses, deliberately: behind a proxy RemoteAddr is the proxy on
		// every request, so recording it alone would answer nothing. See the
		// migration-720 comment.
		p.RemoteAddr = httpx.RemoteAddrIP(r)
		p.ClientIP = httpx.ClientIP(r, s.trustedProxies)
		p.UserAgent = httpx.UserAgent(r)
	}
	if err := auditlog.WriteAuthEvent(ctx, s.db, p); err != nil {
		s.log.Error("audit: auth event not recorded", "kind", p.Kind, "error", err)
	}
}

// auditThrottled records an event at most once per key per window.
//
// It shares loginSeen with recordLoginThrottled, keyed by a prefix, because the
// problem is identical: in trusted-header mode identity is resolved on every
// request, so anything derived from it fires per request. Without this, one
// browser tab polling the API would write thousands of identical "login" rows a
// day and bury the events an auditor is actually looking for.
func (s *Service) auditThrottled(ctx context.Context, r *http.Request, key string, p auditlog.AuthEventParams) {
	if !s.throttleAllow(key) {
		return
	}
	s.auditAuth(ctx, r, p)
}

// throttleAllow reports whether key has not fired within loginRecordWindow, and
// records this firing if so.
func (s *Service) throttleAllow(key string) bool {
	now := time.Now().UTC()
	s.loginSeenMu.Lock()
	defer s.loginSeenMu.Unlock()
	if last, seen := s.loginSeen[key]; seen && now.Sub(last) < loginRecordWindow {
		return false
	}
	s.loginSeen[key] = now
	return true
}

// AuditDenied records an authorization denial for an already-authenticated
// caller. Exported so the API layer's scope and permission guards — which live
// in internal/api, not here — record through the same writer and the same
// address derivation.
//
// reason is a stable machine token (insufficient_scope, insufficient_role,
// insufficient_permission); target names the scope, role or permission that was
// required. Denials are NOT throttled: each one is a distinct decision about a
// distinct resource, and collapsing them would hide exactly the enumeration
// pattern this exists to reveal.
func (s *Service) AuditDenied(r *http.Request, actor, reason, target, details string) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	s.auditAuth(ctx, r, auditlog.AuthEventParams{
		Kind:    auditlog.AuthDenied,
		Outcome: auditlog.OutcomeFailure,
		Actor:   actor,
		Reason:  reason,
		Target:  target,
		Details: details,
	})
}
