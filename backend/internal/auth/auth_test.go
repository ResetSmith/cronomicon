package auth

import (
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

// testService builds an oidc-mode service: these tests exercise the cookie
// session path, which is the optional OIDC mode (RequireSession reads the
// session cookie). Header-mode behavior is covered in header_test.go.
func testService(t *testing.T) *Service {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	codec, _ := newSessionCodec(nil, nil, false) // ephemeral keys are fine for tests
	return &Service{
		db:        pool,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		codec:     codec,
		mode:      config.AuthModeOIDC,
		loginSeen: make(map[string]time.Time),
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := testService(t)
	want := Identity{Email: "a@b.com", DisplayName: "A B", Roles: []string{"admin"}, AllowedScopes: []string{"Prod"}}

	rec := httptest.NewRecorder()
	if err := s.codec.write(rec, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	got, ok := s.codec.read(req)
	if !ok {
		t.Fatal("session did not decode")
	}
	if got.Email != want.Email || !got.HasRole("admin") || len(got.AllowedScopes) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRequireSessionGate(t *testing.T) {
	s := testService(t)
	reached := false
	h := s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := IdentityFrom(r.Context()); !ok {
			t.Error("identity missing from context inside protected handler")
		}
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// No cookie → 401, handler not reached.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("unauth: code=%d reached=%v", rec.Code, reached)
	}

	// Valid cookie → handler reached.
	wr := httptest.NewRecorder()
	_ = s.codec.write(wr, Identity{Email: "x@y.com"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for _, c := range wr.Result().Cookies() {
		req.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK || !reached {
		t.Fatalf("auth: code=%d reached=%v", rec2.Code, reached)
	}
}

func TestRequireCSRF(t *testing.T) {
	s := testService(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.RequireCSRF(ok)

	// Safe method passes without a token.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET should pass, got %d", rec.Code)
	}

	// POST without token → 403.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF should be 403, got %d", rec.Code)
	}

	// POST with matching cookie + header → pass.
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok123"})
	req.Header.Set(csrfHeaderName, "tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with matching CSRF should pass, got %d", rec.Code)
	}

	// Mismatched header → 403.
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok123"})
	req.Header.Set(csrfHeaderName, "WRONG")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched CSRF should be 403, got %d", rec.Code)
	}
}

func TestRequireRunner(t *testing.T) {
	s := testService(t)
	insert := func(token string, expires time.Time, revoked bool) {
		var rev any
		if revoked {
			rev = time.Now().UTC().Format(time.RFC3339)
		}
		_, err := s.db.Exec(`INSERT INTO runner_tokens(token_hash,created_by,created_at,expires_at,revoked_at) VALUES(?,?,?,?,?)`,
			HashToken(token), "test", time.Now().UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339), rev)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("good", time.Now().Add(time.Hour), false)
	insert("expired", time.Now().Add(-time.Hour), false)
	insert("revoked", time.Now().Add(time.Hour), true)

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.RequireRunner(ok)

	cases := map[string]int{"": 401, "good": 200, "expired": 401, "revoked": 401, "nonsense": 401}
	for tok, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/poll", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("token %q: got %d want %d", tok, rec.Code, want)
		}
	}
}
