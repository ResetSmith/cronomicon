package secrets

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestVaultClientDoesNotFollowRedirectWithToken (SU-8): Go's stdlib does not strip
// custom headers on a cross-host redirect, so a Vault upstream that 3xx-redirects a
// KV read to an attacker host would leak X-Vault-Token. The client sets
// CheckRedirect to ErrUseLastResponse, so the token never reaches the redirect
// target.
func TestVaultClientDoesNotFollowRedirectWithToken(t *testing.T) {
	var attackerHits atomic.Int32
	var attackerGotToken atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		if r.Header.Get("X-Vault-Token") != "" {
			attackerGotToken.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(attacker.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "tok-abc", "lease_duration": 3600},
		})
	})
	// A compromised/misconfigured upstream: the KV read redirects to the attacker.
	mux.HandleFunc("/v1/secret/data/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	})
	vault := httptest.NewServer(mux)
	t.Cleanup(vault.Close)

	vc := loopbackVaultClient(vault.URL, "role-1", "secret-1")
	// The fetch fails (a 302 instead of a secret); we only care that the token was
	// never delivered to the redirect target.
	_, _ = vc.Fetch("secret/data/cronomicon/app#DB_PASSWORD")

	if attackerGotToken.Load() {
		t.Errorf("X-Vault-Token was leaked to the redirect target")
	}
	if hits := attackerHits.Load(); hits != 0 {
		t.Errorf("client followed the redirect to the attacker host (%d hits); CheckRedirect must stop it", hits)
	}
}
