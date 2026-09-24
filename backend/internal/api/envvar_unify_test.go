package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestEnvVarSecretCrossTableUniqueness covers A13 (Phase 6): a key must be unique
// ACROSS env_vars + secrets within a scope, enforced at write time in both
// directions (each table already enforces its own UNIQUE(key, scope)).
func TestEnvVarSecretCrossTableUniqueness(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	post := func(path string, body any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	prod := "Prod"

	// env var SHARED@Prod, then a same-key secret @Prod → 409.
	if c := post("/api/v1/env-vars", map[string]any{"key": "SHARED", "value": "v", "scope": prod}); c != http.StatusCreated {
		t.Fatalf("create env var = %d, want 201", c)
	}
	if c := post("/api/v1/env-secrets", map[string]any{"key": "SHARED", "source": "vault", "vaultPath": "secret/x", "scope": prod}); c != http.StatusConflict {
		t.Errorf("secret colliding with env var = %d, want 409", c)
	}
	// Different scope is allowed (scope is part of the key).
	if c := post("/api/v1/env-secrets", map[string]any{"key": "SHARED", "source": "vault", "vaultPath": "secret/x", "scope": "Dev"}); c != http.StatusCreated {
		t.Errorf("secret SHARED@Dev (env var is @Prod) = %d, want 201 (scope-distinct)", c)
	}

	// Reverse direction: secret first, then a same-key env var → 409.
	if c := post("/api/v1/env-secrets", map[string]any{"key": "TOKEN", "source": "vault", "vaultPath": "secret/y"}); c != http.StatusCreated {
		t.Fatalf("create secret = %d, want 201", c)
	}
	if c := post("/api/v1/env-vars", map[string]any{"key": "TOKEN", "value": "v"}); c != http.StatusConflict {
		t.Errorf("env var colliding with secret = %d, want 409", c)
	}
}
