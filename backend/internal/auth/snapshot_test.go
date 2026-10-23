package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// The grant snapshot (LR-78) and the session it serves.
//
// Until v2.3.0 grants were expanded at login and frozen into the cookie, and
// every RBAC write bumped a global epoch that signed everyone else out. These
// tests were first written as pins of that behaviour (Phase 0 of the LR band)
// and are its inverse now: what a group may do is read per request.

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

// A grant written after login reaches a live session on its next request, and
// a grant removed after login stops working on its next request. The cookie is
// the same cookie throughout: it carries the groups, not what they grant.
func TestAGrantChangeReachesALiveSessionOnItsNextRequest(t *testing.T) {
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
	can := func(scope string) bool {
		t.Helper()
		got, ok := s.readSession(nil, req)
		if !ok {
			t.Fatal("the session should be valid: nothing revokes it")
		}
		return got.Can(PermTriggerJobs, scope)
	}
	if !can("fin-hosts") || can("tax-hosts") {
		t.Fatal("precondition: the session holds FIN and not TAX")
	}

	// A global administrator gives the same group TAX as well.
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	               VALUES ('g2','ops','operator','ag:TAX',0,'t')`)
	GrantsChanged()
	if !can("tax-hosts") {
		t.Error("a grant written after login did not reach the live session")
	}

	// The FIN grant is removed: the session loses it at once, with no sign-out.
	lr0Seed(t, s, `DELETE FROM access_grants WHERE id = 'g1'`)
	GrantsChanged()
	if can("fin-hosts") {
		t.Error("a grant removed after login is still in force for the live session")
	}
	if !can("tax-hosts") {
		t.Error("removing one grant took the other with it")
	}
}

// A scope created, renamed or deleted after login is what the session's agency
// grant covers on the next request. Rename was the case v2.2.2 left broken: the
// cookie held the old name, so an agency administrator who renamed their own
// scope could not use it under either name until they signed in again.
func TestAScopeChangeReachesALiveSessionOnItsNextRequest(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	lr0Seed(t, s,
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:FIN','FIN','t')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:old','fin-old','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:old','ag:FIN')`,
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		 VALUES ('g1','fin-admins','admin','ag:FIN',0,'t')`,
	)
	req := lr0Session(t, s, lr0Login(t, s, "fin@example.com", "fin-admins"))
	scopes := func() []string {
		t.Helper()
		got, ok := s.readSession(nil, req)
		if !ok {
			t.Fatal("the session should be valid")
		}
		return got.AllowedScopes
	}

	lr0Seed(t, s,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:new','fin-new','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:new','ag:FIN')`)
	GrantsChanged()
	if got := scopes(); !slices.Equal(got, []string{"fin-new", "fin-old"}) {
		t.Errorf("after a create: scopes = %v, want [fin-new fin-old]", got)
	}

	lr0Seed(t, s, `UPDATE scopes SET name = 'fin-renamed' WHERE id = 'sc:old'`)
	GrantsChanged()
	if got := scopes(); !slices.Equal(got, []string{"fin-new", "fin-renamed"}) {
		t.Errorf("after a rename: scopes = %v, want [fin-new fin-renamed] (the old name must be gone)", got)
	}

	lr0Seed(t, s, `DELETE FROM scopes WHERE id = 'sc:new'`)
	GrantsChanged()
	if got := scopes(); !slices.Equal(got, []string{"fin-renamed"}) {
		t.Errorf("after a delete: scopes = %v, want [fin-renamed]", got)
	}
}

// The cookie carries no authority. A cookie written from an identity that
// claims a role and scopes decodes with neither, and what the session may do is
// whatever its groups grant — here nothing, because no grant names them.
func TestTheCookieCarriesNoGrants(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	forged := Identity{
		Email: "a@b.com", Groups: []string{"nobody-maps-this"},
		Roles: []string{"admin"}, AllowedScopes: []string{AllScopes},
		Grants: []RoleGrant{{Role: "admin", Scopes: []string{AllScopes}}},
		Epoch:  s.currentSessionEpoch(),
	}
	req := lr0Session(t, s, forged)

	raw, ok := s.codec.read(req)
	if !ok {
		t.Fatal("the cookie did not decode")
	}
	if len(raw.Roles) != 0 || len(raw.AllowedScopes) != 0 || len(raw.Grants) != 0 {
		t.Errorf("the cookie carried authority: roles=%v scopes=%v grants=%v", raw.Roles, raw.AllowedScopes, raw.Grants)
	}
	got, ok := s.readSession(nil, req)
	if !ok {
		t.Fatal("the session should be valid")
	}
	if got.Unrestricted() || got.CanAnywhere(PermConfigureApp) || len(got.Grants) != 0 {
		t.Errorf("an identity whose groups hold no grant was authorised: %+v", got)
	}
}

// The developer login is not resolved from grants: its group has an
// access_grants row only when the demo seed ran. It keeps its constructed "*"
// grant while dev auth is on, and is an ordinary (grantless) session when off.
func TestTheDevIdentityKeepsItsConstructedGrant(t *testing.T) {
	s := testService(t)
	s.devAuth = true
	s.loadSessionEpoch(context.Background())
	dev := devIdentity()
	dev.Epoch = s.currentSessionEpoch()
	req := lr0Session(t, s, dev)

	got, ok := s.readSession(nil, req)
	if !ok || !got.Unrestricted() || !got.GlobalAdmin(PermConfigureApp) {
		t.Fatalf("the dev session lost its access on an unseeded database: ok=%v grants=%v", ok, got.Grants)
	}
	s.devAuth = false
	if got, ok := s.readSession(nil, req); ok && got.CanAnywhere(PermConfigureApp) {
		t.Error("with dev auth off the developer email must not be granted anything by name")
	}
}

// A change this process did not make (`cronomicon grant-admin`, a row edited
// by hand) announces nothing. The TTL is what finds it.
//
// The counter is process-wide, so this test's middle assertion holds only
// because no test in this package runs in parallel and calls GrantsChanged.
func TestTheTTLFindsAChangeNobodyAnnounced(t *testing.T) {
	s := testService(t)
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g1','ops','viewer',NULL,1,'t')`)
	ctx := context.Background()
	if g, _ := s.grantsFor(ctx, []string{"rescue"}); len(g) != 0 {
		t.Fatalf("precondition: %v", g)
	}
	// Written with no GrantsChanged call, as another process would.
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g2','rescue','admin',NULL,1,'t')`)
	if g, _ := s.grantsFor(ctx, []string{"rescue"}); len(g) != 0 {
		t.Fatal("the snapshot was rebuilt with no announcement and no TTL expiry; the fast path is not being taken")
	}
	// Age the snapshot past its TTL.
	cur := *s.grants.cur.Load()
	cur.builtAt = time.Now().Add(-grantSnapshotTTL - time.Second)
	s.grants.cur.Store(&cur)
	if g, _ := s.grantsFor(ctx, []string{"rescue"}); len(g) != 1 || !g[0].Unrestricted() {
		t.Errorf("after the TTL the unannounced grant = %v, want the admin grant", g)
	}
}

// A rebuild that fails keeps the last good snapshot in force — for a bounded
// time. Past grantSnapshotMaxStale the session is refused: the snapshot is
// known to be out of date, and the change it is missing may be a revocation.
// With no snapshot at all, resolution fails at once.
func TestAFailedRebuildKeepsTheLastGoodSnapshotForABoundedTime(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g1','ops','admin',NULL,1,'t')`)
	ctx := context.Background()
	req := lr0Session(t, s, lr0Login(t, s, "op@example.com", "ops"))
	if _, ok := s.readSession(nil, req); !ok {
		t.Fatal("precondition: the session resolves")
	}

	// The database goes away and a writer had announced a change.
	_ = s.db.Close()
	GrantsChanged()
	got, ok := s.readSession(nil, req)
	if !ok || !got.GlobalAdmin(PermConfigureApp) {
		t.Errorf("a failed rebuild dropped the last good snapshot: ok=%v grants=%v", ok, got.Grants)
	}
	// Between attempts (the backoff) the answer is the same.
	if _, ok := s.readSession(nil, req); !ok {
		t.Error("the last good snapshot was dropped during the rebuild backoff")
	}

	// The outage outlasts the bound: refused, on a fresh attempt and between attempts.
	s.grants.mu.Lock()
	s.grants.failingSince = time.Now().Add(-grantSnapshotMaxStale - time.Second)
	s.grants.mu.Unlock()
	if _, ok := s.readSession(nil, req); ok {
		t.Error("a snapshot known to be stale for longer than the bound still authorised a session (in backoff)")
	}
	s.grants.mu.Lock()
	s.grants.lastFailed = time.Now().Add(-2 * grantRebuildBackoff)
	s.grants.mu.Unlock()
	if _, ok := s.readSession(nil, req); ok {
		t.Error("a snapshot known to be stale for longer than the bound still authorised a session (after a retry)")
	}

	// A Service that never had a snapshot cannot authorise anyone.
	cold := &Service{db: s.db, log: s.log, codec: s.codec, mode: s.mode, loginSeen: map[string]time.Time{}}
	if _, err := cold.grantsFor(ctx, []string{"ops"}); err == nil {
		t.Error("resolution succeeded with no snapshot and no database")
	}
	if _, ok := cold.readSession(nil, req); ok {
		t.Error("a session was admitted although its grants could not be resolved")
	}
}

// The rebuild is not the triggering request's to cancel. A client that has
// already hung up must not leave the pre-revocation snapshot in force for
// everyone else — otherwise a revoked user keeps their grant alive by aborting
// requests.
func TestACancelledRequestDoesNotKeepARevokedGrantAlive(t *testing.T) {
	s := testService(t)
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g1','ops','admin',NULL,1,'t')`)
	if g, err := s.grantsFor(context.Background(), []string{"ops"}); err != nil || len(g) != 1 {
		t.Fatalf("precondition: %v, %v", g, err)
	}

	lr0Seed(t, s, `DELETE FROM access_grants WHERE id = 'g1'`)
	GrantsChanged()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	// The first request after the revocation belongs to a client that has gone.
	if g, err := s.grantsFor(gone, []string{"ops"}); err != nil || len(g) != 0 {
		t.Errorf("the cancelled request was answered from the old snapshot: grants=%v err=%v", g, err)
	}
	// And the next one, at once, sees the revocation.
	if g, _ := s.grantsFor(context.Background(), []string{"ops"}); len(g) != 0 {
		t.Errorf("the revoked grant is still in force for the next request: %v", g)
	}
}

// Readers and writers race for real under -race: many requests resolving
// while changes are announced. Every answer must be one of the two states the
// database was in, never a mixture.
func TestTheSnapshotIsSafeUnderConcurrentChange(t *testing.T) {
	s := testService(t)
	lr0Seed(t, s,
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:FIN','FIN','t')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('s1','fin-a','cronomicon','t'), ('s2','fin-b','cronomicon','t')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s1','ag:FIN'), ('s2','ag:FIN')`,
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g1','ops','operator','ag:FIN',0,'t')`,
	)
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g, err := s.grantsFor(ctx, []string{"ops"})
				if err != nil {
					t.Errorf("resolve: %v", err)
					return
				}
				if len(g) != 1 || (len(g[0].Scopes) != 1 && len(g[0].Scopes) != 2) {
					t.Errorf("an answer that matches neither state: %+v", g)
					return
				}
				// A caller may do as it likes with what it is handed.
				g[0].Scopes = append(g[0].Scopes[:0], "scribbled")
			}
		}()
	}
	for i := range 40 {
		q := `DELETE FROM scope_agencies WHERE scope_id = 's2'`
		if i%2 == 1 {
			q = `INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s2','ag:FIN')`
		}
		lr0Seed(t, s, q)
		GrantsChanged()
	}
	close(stop)
	wg.Wait()
	if g, _ := s.grantsFor(ctx, []string{"ops"}); len(g) != 1 || !slices.Equal(g[0].Scopes, []string{"fin-a", "fin-b"}) {
		t.Errorf("after the churn: %+v, want the grant on [fin-a fin-b] (a caller's scribble reached the snapshot?)", g)
	}
}

// RevokeSessions is the one remaining way to sign everyone out (LR-79): the
// caller keeps a session at the new epoch, everyone else's cookie is refused.
func TestRevokeSessionsSignsOutEveryoneButTheCaller(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	               VALUES ('g1','root','admin',NULL,1,'t'), ('g2','ops','operator',NULL,1,'t')`)
	root := lr0Session(t, s, lr0Login(t, s, "root@example.com", "root"))
	other := lr0Session(t, s, lr0Login(t, s, "op@example.com", "ops"))

	out := httptest.NewRecorder()
	s.RevokeSessions(out, root)
	if out.Code != http.StatusNoContent {
		t.Fatalf("RevokeSessions = %d, want 204", out.Code)
	}
	if _, ok := s.readSession(nil, other); ok {
		t.Error("another user's session survived the revocation")
	}
	if _, ok := s.readSession(nil, root); ok {
		t.Error("the caller's OLD cookie must be refused too; only the re-issued one is valid")
	}
	next := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range out.Result().Cookies() {
		next.AddCookie(c)
	}
	if got, ok := s.readSession(nil, next); !ok || !got.GlobalAdmin(PermManageRoles) {
		t.Error("the caller was signed out of the request that succeeded")
	}
}

// A revocation that did not happen must not say it did: this route exists for
// the moment an administrator needs someone signed out.
func TestRevokeSessionsReportsAFailedRevocation(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	lr0Seed(t, s, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g1','root','admin',NULL,1,'t')`)
	root := lr0Session(t, s, lr0Login(t, s, "root@example.com", "root"))
	before := s.currentSessionEpoch()

	_ = s.db.Close()
	out := httptest.NewRecorder()
	s.RevokeSessions(out, root)
	if out.Code != http.StatusInternalServerError {
		t.Errorf("RevokeSessions with the epoch unwritable = %d, want 500", out.Code)
	}
	if s.currentSessionEpoch() != before {
		t.Error("the in-memory epoch moved although the stored one did not")
	}
	if len(out.Result().Cookies()) != 0 {
		t.Error("the caller's cookie was re-issued for a revocation that did not happen")
	}
}

// What the bootstrap group grants is shown as such, and the group is not listed
// as one that "matched nothing".
func TestMyAccessNamesTheBootstrapGrant(t *testing.T) {
	s := headerService(t, "192.0.2.0/24", "Break-Glass")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/access", nil)
	req.Header.Set("Remote-User", "rescue@example.com")
	req.Header.Set("Remote-Groups", "break-glass,staff")
	id, ok := s.identityFromHeaders(req.Context(), req)
	if !ok {
		t.Fatal("the bootstrap member did not authenticate")
	}
	rec := httptest.NewRecorder()
	s.MyAccess(rec, req.WithContext(WithIdentity(req.Context(), id)))
	var got myAccess
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(got.Grants) != 1 || got.Grants[0].Origin != "bootstrap" || !got.Grants[0].AllScopes ||
		!slices.Equal(got.Grants[0].Groups, []string{"break-glass"}) {
		t.Errorf("grants = %+v, want the one bootstrap grant attributed to break-glass", got.Grants)
	}
	if !slices.Equal(got.UnmatchedGroups, []string{"staff"}) {
		t.Errorf("unmatched groups = %v, want [staff] (the bootstrap group supplies access)", got.UnmatchedGroups)
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

// Medians of 5 × 2000 on one host (noisy to about ±10%), before and after
// grants moved from the cookie to the snapshot (LR-78, 2026-10-07):
//
//	                        before      after
//	IdentityFromCookie      60,600     49,600 ns/op   a smaller cookie to decrypt; one snapshot lookup
//	IdentityFromHeaders    112,800     12,000 ns/op   two queries per request became none
//
// The budget was "the cookie path no slower, the header path down towards it".
//
// BenchmarkIdentityFromCookie is the OIDC path: decrypt the cookie, check the
// epoch and the idle policy, resolve the groups through the snapshot.
//
//	go test ./internal/auth/ -run '^$' -bench BenchmarkIdentityFrom -benchtime 2000x -count 5
func BenchmarkIdentityFromCookie(b *testing.B) {
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

// BenchmarkIdentityFromHeaders is the trusted-header path: the same snapshot
// lookup, where v2.2 ran two queries per request (access_grants, then
// scope_agencies ⋈ scopes).
func BenchmarkIdentityFromHeaders(b *testing.B) {
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
