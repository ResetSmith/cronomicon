package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// LR Phase 0 — today's behaviour, pinned before Phase S changes it.
//
// Grants are expanded to scope names at login and frozen into the session
// cookie (RB-Q10). LR-78 resolves them per request from an in-memory snapshot
// instead, and LR-79 removes the epoch bump that existed to paper over the
// freeze. Each test below states what is true NOW and names the phase that
// inverts it; the benchmarks record what an authorised request costs on the two
// paths that exist, as the budget the snapshot has to meet.

// lr0Session writes id into a session cookie and returns a request carrying it.
func lr0Session(t testing.TB, s *Service, id Identity) *http.Request {
	t.Helper()
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

// lr0Login builds the identity a login would issue for the given groups.
func lr0Login(t testing.TB, s *Service, email string, groups ...string) Identity {
	t.Helper()
	grants, err := ResolveGrants(context.Background(), s.db, groups)
	if err != nil {
		t.Fatalf("resolve grants: %v", err)
	}
	return Identity{
		Email: email, Groups: groups, Grants: grants,
		Roles: UnionGrantRoles(grants), AllowedScopes: UnionGrantScopes(grants),
		Epoch: s.currentSessionEpoch(),
	}
}

func lr0Seed(t testing.TB, s *Service, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
}

// A grant written after login does not reach a live session: the cookie carries
// the grants as they were resolved, and nothing re-reads them.
//
// Phase S inverts this: the grant takes effect on the session's next request.
func TestLR0_ALiveSessionDoesNotSeeAGrantWrittenAfterLogin(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	lr0Seed(t, s,
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:FIN','FIN','t'), ('ag:TAX','TAX','t')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:fin','fin-hosts','cronomicon','t'), ('sc:tax','tax-hosts','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:fin','ag:FIN'), ('sc:tax','ag:TAX')`,
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		 VALUES ('g1','ops','operator','ag:FIN',0,'t')`,
	)
	req := lr0Session(t, s, lr0Login(t, s, "op@example.com", "ops"))

	// A global administrator gives the same group TAX as well. The write is made
	// directly: this test is about what the session reads, not about the epoch
	// bump the route adds (the next test).
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	               VALUES ('g2','ops','operator','ag:TAX',0,'t')`)

	got, ok := s.readSession(nil, req)
	if !ok {
		t.Fatal("the session should still be valid: no epoch was bumped")
	}
	if !got.Can(PermTriggerJobs, "fin-hosts") {
		t.Fatal("precondition: the session holds its login-time grant")
	}
	if got.Can(PermTriggerJobs, "tax-hosts") {
		t.Error("PIN: a live cookie session picked up a grant written after login. " +
			"That is Phase S's goal (LR-78); if it is now true, replace this pin with its inverse.")
	}

	// A grant REMOVED after login is the same freeze in the dangerous direction,
	// and it is why every RBAC write bumps the epoch.
	lr0Seed(t, s, `DELETE FROM access_grants WHERE id = 'g1'`)
	if got, _ := s.readSession(nil, req); !got.Can(PermTriggerJobs, "fin-hosts") {
		t.Error("PIN: a live cookie session lost a grant without an epoch bump (LR-78).")
	}
}

// One agency's edit signs every other user out. The acting administrator keeps
// a session (re-issued at the new epoch); everyone else's cookie is refused on
// its next request, whichever agency they belong to.
//
// Phase S keeps the epoch check and removes its RBAC callers (LR-79), so at the
// route level this stops being reachable; api.TestLR0_AScopeCreateSignsOutEveryOtherSession
// is the pin that inverts.
func TestLR0_AnRBACEditInOneAgencyRevokesEveryOtherSession(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	lr0Seed(t, s,
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:FIN','FIN','t'), ('ag:TAX','TAX','t')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:fin','fin-hosts','cronomicon','t'), ('sc:tax','tax-hosts','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:fin','ag:FIN'), ('sc:tax','ag:TAX')`,
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		 VALUES ('g1','fin-admins','admin','ag:FIN',0,'t'), ('g2','tax-ops','operator','ag:TAX',0,'t')`,
	)
	finAdmin := lr0Session(t, s, lr0Login(t, s, "fin@example.com", "fin-admins"))
	taxOp := lr0Session(t, s, lr0Login(t, s, "tax@example.com", "tax-ops"))

	// FIN's administrator changes something in FIN.
	s.RevokeOtherSessionsRefreshingOwn(httptest.NewRecorder(), finAdmin)

	if _, ok := s.readSession(nil, taxOp); ok {
		t.Error("PIN: an operator in TAX kept their session across an edit in FIN. " +
			"That is LR-79's goal; if the RBAC writers no longer bump the epoch, replace this pin.")
	}
}

// benchService is testService for a benchmark (testService takes a *testing.T).
func benchService(b *testing.B) *Service {
	b.Helper()
	pool, err := db.Open(filepath.Join(b.TempDir(), "auth.db"))
	if err != nil {
		b.Fatalf("open db: %v", err)
	}
	b.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	codec, _ := newSessionCodec(nil, nil, false)
	return &Service{
		db:        pool,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		codec:     codec,
		mode:      config.AuthModeOIDC,
		loginSeen: make(map[string]time.Time),
	}
}

// lr0BenchFixture is a mid-sized installation: 20 agencies of 10 scopes, and a
// user whose three groups hold a role on three of them.
func lr0BenchFixture(b *testing.B, s *Service) []string {
	b.Helper()
	for a := range 20 {
		lr0Seed(b, s, fmt.Sprintf(`INSERT INTO agencies (id,name,created_at) VALUES ('ag:%d','A%d','t')`, a, a))
		for sc := range 10 {
			lr0Seed(b, s,
				fmt.Sprintf(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:%d:%d','a%d-scope-%d','cronomicon','t')`, a, sc, a, sc),
				fmt.Sprintf(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:%d:%d','ag:%d')`, a, sc, a))
		}
		lr0Seed(b, s, fmt.Sprintf(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		                           VALUES ('g:%d','group-%d','operator','ag:%d',0,'t')`, a, a, a))
	}
	return []string{"group-3", "group-7", "group-11"}
}

// BASELINE, 2026-10-07, before LR-78 (medians of 5 × 2000; this host is noisy to
// about ±10%):
//
//	IdentityFromCookie     60,600 ns/op   decode, epoch, idle policy; no grant query
//	IdentityFromHeaders   112,800 ns/op   two grant queries per request
//
// The budget for the snapshot: the cookie path no slower than this beyond
// noise, the header path down towards it.
//
// BenchmarkLR0_IdentityFromCookie is the OIDC path today: decrypt the cookie,
// check the epoch, no database read for grants. LR-78 moves grants out of the
// cookie, so this is the number the snapshot lookup is compared with.
//
//	go test ./internal/auth/ -run '^$' -bench BenchmarkLR0 -benchtime 2000x -count 5
func BenchmarkLR0_IdentityFromCookie(b *testing.B) {
	s := benchService(b)
	groups := lr0BenchFixture(b, s)
	req := lr0Session(b, s, lr0Login(b, s, "u@example.com", groups...))
	b.ResetTimer()
	for b.Loop() {
		id, ok := s.readSession(nil, req)
		if !ok || !id.Can(PermTriggerJobs, "a7-scope-4") {
			b.Fatal("the session did not authorise")
		}
	}
}

// BenchmarkLR0_IdentityFromHeaders is the trusted-header path today: two
// queries per request (access_grants, then scope_agencies ⋈ scopes). LR-78
// replaces those with the same snapshot lookup, so this one should fall.
func BenchmarkLR0_IdentityFromHeaders(b *testing.B) {
	s := benchService(b)
	groups := lr0BenchFixture(b, s)
	s.headers = headerNames{user: "Remote-User", email: "Remote-Email", name: "Remote-Name", groups: "Remote-Groups"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Remote-User", "u@example.com")
	req.Header.Set("Remote-Groups", groups[0]+","+groups[1]+","+groups[2])
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		id, ok := s.identityFromHeaders(ctx, req)
		if !ok || !id.Can(PermTriggerJobs, "a7-scope-4") {
			b.Fatal("the headers did not authorise")
		}
	}
}
