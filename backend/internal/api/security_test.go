package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// get issues a GET against the test server with optional headers and returns the
// status code (body closed).
func get(t *testing.T, base, path string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestSpoofedHeaderRejectedAtServer is the single most important deployment test
// (D.1): a client that is not in AMADEUS_TRUSTED_PROXIES cannot authenticate by
// sending Remote-* headers — they are stripped and the request is unauthenticated.
func TestSpoofedHeaderRejectedAtServer(t *testing.T) {
	spoofed := map[string]string{
		"Remote-User":   "attacker",
		"Remote-Email":  "attacker@evil.test",
		"Remote-Groups": "amadeus-admins,admins",
	}

	t.Run("untrusted peer: spoofed headers stripped → 401", func(t *testing.T) {
		// Trust only 10.0.0.0/8; the test client connects from loopback, so it is
		// NOT trusted and its Remote-* must be ignored.
		h := serverForMode(t, &config.Config{
			AuthMode:       config.AuthModeTrustedHeader,
			TrustedProxies: []string{"10.0.0.0/8"},
		})
		ts := httptest.NewServer(h)
		defer ts.Close()
		if code := get(t, ts.URL, "/api/v1/me", spoofed); code != http.StatusUnauthorized {
			t.Fatalf("spoofed Remote-* from untrusted peer: got %d, want 401", code)
		}
	})

	t.Run("trusted peer: headers honored → 200", func(t *testing.T) {
		// Trust loopback (both IPv4 and IPv6 forms) so the same headers are accepted
		// — this simulates the request arriving from the reverse proxy.
		h := serverForMode(t, &config.Config{
			AuthMode:       config.AuthModeTrustedHeader,
			TrustedProxies: []string{"127.0.0.0/8", "::1/128"},
		})
		ts := httptest.NewServer(h)
		defer ts.Close()
		if code := get(t, ts.URL, "/api/v1/me", spoofed); code != http.StatusOK {
			t.Fatalf("Remote-* from trusted peer: got %d, want 200", code)
		}
	})
}

// TestUnauthenticatedSurface (D.1 surface check) confirms exactly which routes are
// reachable without authentication: health/readiness/version/metrics + the public
// providers endpoint. Every operator route must reject (401).
func TestUnauthenticatedSurface(t *testing.T) {
	h := serverForMode(t, &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"10.0.0.0/8"}, // loopback untrusted ⇒ no ambient identity
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Intentionally open (no auth): must NOT be 401.
	for _, path := range []string{"/healthz", "/readyz", "/version", "/metrics", "/api/v1/auth/providers"} {
		if code := get(t, ts.URL, path, nil); code == http.StatusUnauthorized {
			t.Errorf("open route %s returned 401 (should be reachable)", path)
		}
	}

	// Operator routes: must reject without an authenticated identity.
	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/env-vars",
		"/api/v1/env-secrets",
		"/api/v1/scopes",
		"/api/v1/runners",
		"/api/v1/settings/general",
		"/api/v1/capabilities",
		"/api/v1/recent-logins",
	} {
		if code := get(t, ts.URL, path, nil); code != http.StatusUnauthorized {
			t.Errorf("operator route %s returned %d, want 401", path, code)
		}
	}
}

// TestDevLoginMountedOnlyWhenEnabled (D.1) confirms the dev bypass route does not
// exist unless AMADEUS_DEV_AUTH is set — production must not expose it.
func TestDevLoginMountedOnlyWhenEnabled(t *testing.T) {
	off := serverForMode(t, &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	tsOff := httptest.NewServer(off)
	defer tsOff.Close()
	if code := get(t, tsOff.URL, "/api/v1/auth/dev-login", nil); code != http.StatusNotFound {
		t.Errorf("dev-login with DevAuth off: got %d, want 404 (route absent)", code)
	}

	on := serverForMode(t, &config.Config{AuthMode: config.AuthModeTrustedHeader, DevAuth: true})
	tsOn := httptest.NewServer(on)
	defer tsOn.Close()
	// dev-login mints a session and 302-redirects; don't follow the redirect (the
	// test server has no SPA at "/"). A mounted route returns 302, not 404.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(tsOn.URL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("dev-login with DevAuth on: got 404, route should be mounted")
	}
}
