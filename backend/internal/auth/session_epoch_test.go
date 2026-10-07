package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSessionEpochRevocation (SU-5): a session cookie stamped at the current epoch
// is accepted; after BumpSessionEpoch (an RBAC change) the same cookie is revoked;
// a fresh login at the new epoch is accepted again. Also confirms graceful upgrade —
// an epoch-0 cookie is valid while the epoch is still 0 (no forced re-login).
func TestSessionEpochRevocation(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	if s.currentSessionEpoch() != 0 {
		t.Fatalf("initial epoch = %d, want 0", s.currentSessionEpoch())
	}

	cookieFor := func(id Identity) *http.Request {
		rec := httptest.NewRecorder()
		if err := s.codec.write(rec, id); err != nil {
			t.Fatalf("write cookie: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		return req
	}

	// A session at the current (0) epoch — also the legacy-cookie case — is accepted.
	stale := cookieFor(Identity{Email: "a@b.com", Epoch: s.currentSessionEpoch()})
	if _, ok := s.readSession(nil, stale); !ok {
		t.Error("current-epoch (and legacy epoch-0) session should be accepted")
	}

	// An RBAC change bumps the epoch; the prior session is now stale.
	s.BumpSessionEpoch(context.Background())
	if s.currentSessionEpoch() != 1 {
		t.Fatalf("epoch after bump = %d, want 1", s.currentSessionEpoch())
	}
	if _, ok := s.readSession(nil, stale); ok {
		t.Error("stale-epoch session must be revoked after a bump")
	}

	// A re-login at the new epoch is accepted again.
	fresh := cookieFor(Identity{Email: "a@b.com", Epoch: s.currentSessionEpoch()})
	if _, ok := s.readSession(nil, fresh); !ok {
		t.Error("re-login at the new epoch should be accepted")
	}
}

// GC-6 — the creator of a scope must be able to use it in the session that made
// it. A scope list is expanded at login and frozen in the cookie, so re-issuing
// the actor's cookie unchanged (RevokeOtherSessions) left them unable to read
// the scope they had just created until they signed in again.
func TestRevokeOtherSessionsRefreshingOwnPicksUpANewScope(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	s.loadSessionEpoch(ctx)
	for _, q := range []string{
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:FIN','FIN','t')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:old','fin-old','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:old','ag:FIN')`,
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		 VALUES ('g1','fin-admins','admin','ag:FIN',0,'t')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// The session as issued at login: one scope.
	grants, err := ResolveGrants(ctx, s.db, []string{"fin-admins"})
	if err != nil {
		t.Fatal(err)
	}
	atLogin := Identity{
		Email: "fin@example.com", Groups: []string{"fin-admins"}, Grants: grants,
		Roles: UnionGrantRoles(grants), AllowedScopes: UnionGrantScopes(grants), Epoch: s.currentSessionEpoch(),
	}
	if atLogin.Can(PermConfigureApp, "fin-new") {
		t.Fatal("precondition: the scope does not exist yet")
	}
	rec := httptest.NewRecorder()
	if err := s.codec.write(rec, atLogin); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}

	// The actor creates a scope in their agency...
	for _, q := range []string{
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:new','fin-new','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:new','ag:FIN')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	out := httptest.NewRecorder()
	s.RevokeOtherSessionsRefreshingOwn(out, req)

	// ...and the cookie they get back can use it, at the new epoch.
	next := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range out.Result().Cookies() {
		next.AddCookie(c)
	}
	got, ok := s.readSession(nil, next)
	if !ok {
		t.Fatal("the actor's own session must survive the bump")
	}
	if !got.Can(PermConfigureApp, "fin-new") || !ScopeReadable(got, "fin-new") {
		t.Errorf("the re-issued session cannot use the new scope: scopes = %v", got.AllowedScopes)
	}
	// Everyone else is still revoked.
	if _, ok := s.readSession(nil, req); ok {
		t.Error("the cookie from before the bump must be revoked")
	}
}
