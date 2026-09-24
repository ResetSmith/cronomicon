package auth

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/config"
)

// Trusted Header SSO (Phase A). The reverse proxy runs forward-auth against the identity provider and
// injects Remote-* headers after authenticating the user. Cronomicon consumes those
// to build an Identity on every request — but ONLY when the request's peer is in
// the trusted-proxy allowlist. The security crux is that these headers must be
// impossible to set from anywhere but the proxy; StripUntrustedHeaders enforces
// that by removing them from every untrusted request before any handler runs.

// StripUntrustedHeaders removes the configured Remote-* identity headers from any
// request whose immediate peer is not in the trusted-proxy allowlist, so a
// spoofed Remote-Groups can never be honored (A.3). It is applied globally,
// before any handler reads identity. Outside trusted-header mode the headers are
// unused, so they are stripped unconditionally (defense in depth).
func (s *Service) StripUntrustedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !(s.mode == config.AuthModeTrustedHeader && s.peerTrusted(r)) {
			r.Header.Del(s.headers.user)
			r.Header.Del(s.headers.email)
			r.Header.Del(s.headers.name)
			r.Header.Del(s.headers.groups)
		}
		next.ServeHTTP(w, r)
	})
}

// peerTrusted reports whether the request's immediate peer (RemoteAddr) is in the
// trusted-proxy allowlist. Cronomicon sits directly behind the proxy on a private
// network, so the immediate peer is the proxy — checking RemoteAddr is the
// load-bearing control. (We deliberately do not trust X-Forwarded-For for the
// trust decision: it is attacker-controlled upstream of the proxy.)
func (s *Service) peerTrusted(r *http.Request) bool {
	if len(s.trustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// identityFromHeaders builds the operator Identity from the (already trust-gated)
// Remote-* headers, resolving roles and allowed scopes from the same DB-driven
// mapping used by OIDC (A.5). Missing the required user header ⇒ unauthenticated
// (the proxy should never let an unauthenticated request through; failing closed
// catches a misconfiguration). Returns ok=false when the request cannot be
// authenticated.
func (s *Service) identityFromHeaders(ctx context.Context, r *http.Request) (Identity, bool) {
	user := strings.TrimSpace(r.Header.Get(s.headers.user))
	email := strings.TrimSpace(r.Header.Get(s.headers.email))
	name := strings.TrimSpace(r.Header.Get(s.headers.name))
	groups := parseGroups(r.Header.Get(s.headers.groups))

	// Require a principal. A forward-auth proxy always sends Remote-User; treat its absence
	// as an unauthenticated/misconfigured request.
	if user == "" && email == "" {
		return Identity{}, false
	}
	// Email is the stable key for recent_logins and role mappings everywhere
	// else; fall back to the username if the proxy omitted the email header.
	principal := email
	if principal == "" {
		principal = user
	}
	display := name
	if display == "" {
		display = user
	}

	// RB-15: grants are AUTHORITATIVE. Roles and AllowedScopes are now DERIVED from
	// them rather than resolved on their own axes — that independence is what
	// produced the cross-product leak.
	//
	// A resolution failure now DOES fail the login (it was tolerated while the field
	// was inert): continuing would hand out an identity with no grants, which after
	// the switch means zero authority. Failing closed on a transient DB error is
	// correct; silently de-privileging every operator is not.
	grants, err := ResolveGrants(ctx, s.db, groups)
	if err != nil {
		s.log.Error("trusted-header grant resolution failed", "error", err)
		return Identity{}, false
	}
	roles := UnionGrantRoles(grants)
	roles, bootstrapped := s.applyBootstrapAdmin(groups, roles)
	// The break-glass floor must reach the grant axis too — the bootstrap group has
	// no grant row by definition, so without this it grants a role name with no
	// authority at all. See WithBootstrapGrant.
	grants = WithBootstrapGrant(grants, bootstrapped)
	if bootstrapped {
		roles = UnionGrantRoles(grants)
	}
	// Visibility stays UNIONED across grants on purpose (§2.2): Alice should SEE
	// both Tax and Finance; she just may not act on Tax. Splitting visibility too
	// would be a far larger change than the one being made.
	scopes := UnionGrantScopes(grants)

	id := Identity{
		Email:         principal,
		DisplayName:   display,
		Groups:        groups,
		Roles:         roles,
		AllowedScopes: scopes,
		Grants:        grants,
		IssuedAt:      time.Now().UTC(),
	}
	s.recordLoginThrottled(ctx, r, id, bootstrapped)
	return id, true
}

// applyBootstrapAdmin grants the admin role to any user in the bootstrap admin
// group, regardless of ad_group_mappings (decision #6 / A.6). It logs a loud
// warning each time it fires so the active bypass is impossible to miss in logs.
// It reports whether the bypass fired, so the caller can audit it — this is a
// standing grant of admin outside the role mappings, which is the single most
// important thing in this file for an auditor to be able to see.
func (s *Service) applyBootstrapAdmin(groups, roles []string) ([]string, bool) {
	if s.bootstrapAdminGroup == "" || !containsFold(groups, s.bootstrapAdminGroup) {
		return roles, false
	}
	if !containsFold(roles, "admin") {
		roles = append(roles, "admin")
	}
	s.log.Warn("bootstrap admin group granted admin — remove CRONOMICON_BOOTSTRAP_ADMIN_GROUP after seeding mappings",
		"group", s.bootstrapAdminGroup)
	return roles, true
}

// recordLoginThrottled upserts the Honest-View row at most once per user per
// window (A.5), since trusted-header identity is resolved on every request.
func (s *Service) recordLoginThrottled(ctx context.Context, r *http.Request, id Identity, bootstrapped bool) {
	now := time.Now().UTC()
	s.loginSeenMu.Lock()
	last, seen := s.loginSeen[id.Email]
	if seen && now.Sub(last) < loginRecordWindow {
		s.loginSeenMu.Unlock()
		return
	}
	s.loginSeen[id.Email] = now
	s.loginSeenMu.Unlock()

	if err := s.recordLogin(ctx, id); err != nil {
		s.log.Error("recording login failed", "error", err, "email", id.Email)
	}
	// LU-9: audit inside the throttle, not beside it. In this mode identity is
	// resolved per request, so an unthrottled event would be one audit row per
	// HTTP request — thousands a day from a single idle browser tab, burying the
	// events an auditor came for.
	s.auditAuth(ctx, r, auditlog.AuthEventParams{
		Kind: auditlog.AuthLogin, Outcome: auditlog.OutcomeSuccess, Actor: id.Email,
		Details: "trusted-header",
	})
	if bootstrapped {
		s.auditAuth(ctx, r, auditlog.AuthEventParams{
			Kind: auditlog.AuthBootstrapAdmin, Outcome: auditlog.OutcomeSuccess,
			Actor: id.Email, Target: s.bootstrapAdminGroup,
			Reason:  "bootstrap_admin_group",
			Details: "admin granted outside ad_group_mappings by CRONOMICON_BOOTSTRAP_ADMIN_GROUP",
		})
	}
}

// parseGroups splits the comma-separated Remote-Groups header into trimmed,
// non-empty group names. Returns nil when the header is absent/empty.
func parseGroups(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}
