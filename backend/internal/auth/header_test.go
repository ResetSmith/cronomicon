package auth

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// headerService builds a trusted-header-mode Service backed by a fresh migrated
// DB. trustedCIDR is the single allowlisted proxy network; bootstrapGroup is the
// optional CRONOMICON_BOOTSTRAP_ADMIN_GROUP.
func headerService(t *testing.T, trustedCIDR, bootstrapGroup string) *Service {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "hdr.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var nets []*net.IPNet
	if trustedCIDR != "" {
		_, n, err := net.ParseCIDR(trustedCIDR)
		if err != nil {
			t.Fatalf("parse cidr: %v", err)
		}
		nets = []*net.IPNet{n}
	}
	codec, _ := newSessionCodec(nil, nil, false)
	return &Service{
		db:    pool,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		codec: codec,
		mode:  config.AuthModeTrustedHeader,
		headers: headerNames{
			user:   "Remote-User",
			email:  "Remote-Email",
			name:   "Remote-Name",
			groups: "Remote-Groups",
		},
		trustedProxies:      nets,
		bootstrapAdminGroup: bootstrapGroup,
		loginSeen:           make(map[string]time.Time),
	}
}

// seedGroupRole grants a group an UNRESTRICTED role.
//
// RB-15: authorization resolves from access_grants — the only access table left
// writes a "*"-shaped grant, and since RB-19 (v0.57.8) that is the only thing it
// writes — the legacy mapping row went with its table. Tests using this helper are
// about header parsing and proxy trust, not about scoping, so unrestricted is the
// right shape for them.
func seedGroupRole(t *testing.T, s *Service, group, role string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO access_grants(id, ad_group, role, agency_id, all_scopes, created_at) VALUES(?,?,?,NULL,1,?)`,
		"g:"+group+":"+role, group, role, now); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// seedScopedGroupRole grants a group a role over ONE named scope, expressed the way
// a real grant is: an agency containing that scope (RB-Q1 — there is no
// single-scope grant shape). Use it where the test is about scope resolution.
func seedScopedGroupRole(t *testing.T, s *Service, group, role, scope string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	agencyID := "ag:" + scope
	scopeID := "sc:" + scope
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO agencies(id, name, created_at) VALUES(?,?,?)`,
		agencyID, "agency-"+scope, now); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO scopes(id, name, source, created_at) VALUES(?,?,'amadeus',?)`,
		scopeID, scope, now); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO scope_agencies(scope_id, agency_id) VALUES(?,?)`,
		scopeID, agencyID); err != nil {
		t.Fatalf("seed scope_agencies: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO access_grants(id, ad_group, role, agency_id, all_scopes, created_at) VALUES(?,?,?,?,0,?)`,
		"g:"+group+":"+role+":"+scope, group, role, agencyID, now); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// TestTrustedProxyStripsSpoofedHeaders is the key security test (A.3/A.10): a
// spoofed Remote-Groups from a peer NOT in the allowlist is dropped, so the
// request is treated as unauthenticated; the same headers from an allowlisted
// peer are honored and resolve to the mapped role.
func TestTrustedProxyStripsSpoofedHeaders(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	seedGroupRole(t, s, "admins", "admin")

	var gotIdentity *Identity
	handler := s.StripUntrustedHeaders(s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := IdentityFrom(r.Context()); ok {
			gotIdentity = &id
		}
		w.WriteHeader(http.StatusOK)
	})))

	newReq := func(remoteAddr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		r.RemoteAddr = remoteAddr
		r.Header.Set("Remote-User", "attacker")
		r.Header.Set("Remote-Groups", "admins")
		return r
	}

	// Spoofed: peer 203.0.113.5 is not in 10.0.0.0/8 → headers stripped → 401.
	gotIdentity = nil
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newReq("203.0.113.5:9999"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed peer: code=%d want 401", rec.Code)
	}
	if gotIdentity != nil {
		t.Fatalf("spoofed peer: identity should not resolve, got %+v", *gotIdentity)
	}

	// Trusted: peer 10.1.2.3 is in the allowlist → headers honored → 200, admin.
	gotIdentity = nil
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, newReq("10.1.2.3:5000"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted peer: code=%d want 200", rec.Code)
	}
	if gotIdentity == nil || !gotIdentity.HasRole("admin") {
		t.Fatalf("trusted peer: expected admin identity, got %+v", gotIdentity)
	}
	if gotIdentity.Email != "attacker" {
		t.Fatalf("trusted peer: principal = %q, want attacker (fallback to Remote-User)", gotIdentity.Email)
	}
}

// TestIdentityFromHeaders covers parsing into Identity and group→role/scope
// resolution from Remote-Groups (A.2/A.5/A.10).
func TestIdentityFromHeaders(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	// Scopes resolve from grants (RB-15), so this is a grant over ONE agency — the
	// one containing Staging-Only. The migration-630 "*" backfill this used to have
	// to clear first went with scope_restrictions itself (RB-19, v0.57.8), so the
	// grant is now the only thing in play. The assertion below is unchanged: the
	// agency expands back to exactly that scope at login, which is the "author on
	// agency, evaluate on scope" property.
	seedScopedGroupRole(t, s, "ops-team", "operator", "Staging-Only")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Remote-User", "jdoe")
	r.Header.Set("Remote-Email", "jdoe@example.com")
	r.Header.Set("Remote-Name", "Jane Doe")
	r.Header.Set("Remote-Groups", " ops-team , other ")

	id, ok := s.identityFromHeaders(context.Background(), r)
	if !ok {
		t.Fatal("expected identity to resolve")
	}
	if id.Email != "jdoe@example.com" || id.DisplayName != "Jane Doe" {
		t.Fatalf("identity fields = %+v", id)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "ops-team" || id.Groups[1] != "other" {
		t.Fatalf("groups parse = %v, want [ops-team other] (trimmed)", id.Groups)
	}
	if !id.HasRole("operator") {
		t.Fatalf("roles = %v, want operator", id.Roles)
	}
	if len(id.AllowedScopes) != 1 || id.AllowedScopes[0] != "Staging-Only" {
		t.Fatalf("scopes = %v, want [Staging-Only]", id.AllowedScopes)
	}

	// Missing both user and email ⇒ unauthenticated (fail closed).
	bare := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	if _, ok := s.identityFromHeaders(context.Background(), bare); ok {
		t.Fatal("expected no identity when required headers absent")
	}
}

// TestIdentityFromHeadersConfigurableNames verifies the header names are
// overridable (A.2).
func TestIdentityFromHeadersConfigurableNames(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	s.headers = headerNames{user: "X-Auth-User", email: "X-Auth-Email", name: "X-Auth-Name", groups: "X-Auth-Groups"}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Auth-User", "svc")
	r.Header.Set("X-Auth-Email", "svc@example.com")

	id, ok := s.identityFromHeaders(context.Background(), r)
	if !ok || id.Email != "svc@example.com" {
		t.Fatalf("configurable header names not honored: ok=%v id=%+v", ok, id)
	}
}

// TestBootstrapAdmin verifies that on a fresh DB with no access grants at all, a
// user in the bootstrap admin group is granted admin (A.6).
func TestBootstrapAdmin(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "amadeus-admins")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Remote-User", "founder@example.com")
	r.Header.Set("Remote-Groups", "amadeus-admins")

	id, ok := s.identityFromHeaders(context.Background(), r)
	if !ok {
		t.Fatal("expected identity to resolve")
	}
	if !id.HasRole("admin") {
		t.Fatalf("bootstrap admin not granted: roles=%v", id.Roles)
	}

	// A user NOT in the bootstrap group gets no admin (and no roles, on a fresh DB).
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Remote-User", "nobody@example.com")
	r2.Header.Set("Remote-Groups", "random-group")
	id2, ok := s.identityFromHeaders(context.Background(), r2)
	if !ok {
		t.Fatal("expected identity to resolve (authenticated, just unprivileged)")
	}
	if id2.HasRole("admin") {
		t.Fatalf("non-bootstrap user should not be admin: roles=%v", id2.Roles)
	}

	// 🔴 The floor must reach the GRANT axis too, not just the role list.
	//
	// This asserts against the real login path rather than the helper, because the
	// failure mode is a login path that resolves grants and forgets to apply the
	// floor. The bootstrap group has no access_grants row BY DEFINITION — it is the
	// mechanism for when the tables are wrong — so once grants become authoritative
	// (RB-15) a bootstrapped admin without this resolves to the role name with zero
	// authority: locked out of the repair surface, with no CLI recovery path.
	if len(id.Grants) == 0 {
		t.Fatal("bootstrap admin resolved NO grants — the break-glass path dies at RB-15")
	}
	unrestrictedAdmin := false
	for _, g := range id.Grants {
		if g.Role == AdminRole && g.Unrestricted() {
			unrestrictedAdmin = true
		}
	}
	if !unrestrictedAdmin {
		t.Fatalf("bootstrap admin has no unrestricted admin grant: %+v", id.Grants)
	}
	// And the floor must not leak to anyone who did not trip it.
	if len(id2.Grants) != 0 {
		t.Fatalf("non-bootstrap user was given grants on a fresh DB: %+v", id2.Grants)
	}
}

// TestRecordLoginThrottle verifies recent_logins is written at most once per
// user per window (A.5).
func TestRecordLoginThrottle(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	id := Identity{Email: "throttle@example.com", DisplayName: "T"}

	s.recordLoginThrottled(context.Background(), nil, id, false)
	s.recordLoginThrottled(context.Background(), nil, id, false) // within window → skipped

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM recent_logins WHERE email = ?`, id.Email).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("recent_logins rows = %d, want 1 (throttled)", n)
	}
}
