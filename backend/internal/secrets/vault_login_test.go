package secrets

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Login (2.5.1 SG3) always reaches the server, and needs no capability beyond
// the AppRole itself: a role whose tokens lack Vault's default policy (no
// lookup-self) must not read as an unreachable Vault. A static token, which
// has no login, is asked about with lookup-self.
func TestLoginReachesTheServerWithoutLookupSelf(t *testing.T) {
	var logins, lookups int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/approle/login"):
			atomic.AddInt32(&logins, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok", "lease_duration": 3600}})
		case strings.HasSuffix(r.URL.Path, "/auth/token/lookup-self"):
			atomic.AddInt32(&lookups, 1)
			if r.Header.Get("X-Vault-Token") != "static-token" {
				// An AppRole token without the default policy.
				http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	approle, err := NewVaultClientWithOptions(srv.URL, "role", "secret", VaultOptions{Egress: loopbackEgress})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := approle.(VaultLogin).Login(); err != nil {
			t.Fatalf("AppRole Login %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&logins); got != 2 {
		t.Errorf("AppRole logins = %d, want 2 (each Login reaches the server, the cached token is not trusted)", got)
	}
	if got := atomic.LoadInt32(&lookups); got != 0 {
		t.Errorf("AppRole Login asked lookup-self %d times, want 0", got)
	}

	static, err := NewVaultClientWithOptions(srv.URL, "", "static-token", VaultOptions{AuthMethod: "token", Egress: loopbackEgress})
	if err != nil {
		t.Fatal(err)
	}
	if err := static.(VaultLogin).Login(); err != nil {
		t.Fatalf("token Login: %v", err)
	}
	if got := atomic.LoadInt32(&lookups); got != 1 {
		t.Errorf("token Login lookups = %d, want 1", got)
	}

	srv.Close()
	if err := approle.(VaultLogin).Login(); err == nil {
		t.Error("Login with the server gone succeeded")
	}
}
