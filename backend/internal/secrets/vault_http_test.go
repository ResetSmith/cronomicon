package secrets

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// loopbackEgress is the SU-7 posture that lets a test dial the httptest fakeVault
// (127.0.0.1) — the guard blocks loopback in production.
var loopbackEgress = httpx.EgressPolicy{AllowPrivate: true, AllowLoopback: true}

// loopbackVaultClient builds a Vault client permitted to dial the loopback
// fakeVault under the SU-7 guard.
func loopbackVaultClient(url, role, secret string) VaultClient {
	c, _ := NewVaultClientWithOptions(url, role, secret, VaultOptions{Egress: loopbackEgress})
	return c
}

// fakeVault is a minimal in-process Vault: AppRole login + KV v2 read/write.
func fakeVault(t *testing.T) (*httptest.Server, map[string]map[string]string) {
	t.Helper()
	store := map[string]map[string]string{} // path → field → value
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != "role-1" || body["secret_id"] != "secret-1" {
			http.Error(w, `{"errors":["invalid role or secret id"]}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "tok-abc", "lease_duration": 3600},
		})
	})
	// KV v2 data path: /v1/secret/data/<...>
	mux.HandleFunc("/v1/secret/data/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok-abc" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Data map[string]string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			store[path] = body.Data
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data := store[path]
			if data == nil {
				http.Error(w, `{"errors":[]}`, http.StatusNotFound)
				return
			}
			inner := map[string]any{}
			for k, v := range data {
				inner[k] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": inner}})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

func TestVaultClientWriteFetchRoundTrip(t *testing.T) {
	srv, _ := fakeVault(t)
	vc := loopbackVaultClient(srv.URL, "role-1", "secret-1")

	ref := "secret/data/cronomicon/app#DB_PASSWORD"
	if err := vc.Write(ref, "s3cr3t"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := vc.Fetch(ref)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("fetch = %q, want s3cr3t", got)
	}

	// A missing field is an explicit error, not a silent empty string.
	if _, err := vc.Fetch("secret/data/cronomicon/app#NOPE"); err == nil {
		t.Error("expected error fetching absent field")
	}
}

func TestVaultClientBadCredentials(t *testing.T) {
	srv, _ := fakeVault(t)
	vc := loopbackVaultClient(srv.URL, "wrong", "wrong")
	if _, err := vc.Fetch("secret/data/x#y"); err == nil {
		t.Error("expected login failure with bad AppRole creds")
	}
}

// TestStubFallbackUnchanged verifies that with no Vault wired, vault-source
// reveals still return the unavailable error (no behavior change — decision #8).
func TestStubFallbackUnchanged(t *testing.T) {
	if _, err := (stubVaultClient{}).Fetch("secret/data/x#y"); err != ErrVaultUnavailable {
		t.Fatalf("stub Fetch = %v, want ErrVaultUnavailable", err)
	}
}

// TestVaultNamespaceHeader (D4): X-Vault-Namespace is sent iff a namespace is
// configured, on every request — dormant otherwise.
func TestVaultNamespaceHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		ns   string
		want string
	}{
		{"configured", "team-a", "team-a"},
		{"unset", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotLogin, gotRead string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/auth/approle/login"):
					gotLogin = r.Header.Get("X-Vault-Namespace")
					_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-abc", "lease_duration": 3600}})
				default:
					gotRead = r.Header.Get("X-Vault-Namespace")
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "v"}}})
				}
			}))
			defer srv.Close()
			vc, err := NewVaultClientWithOptions(srv.URL, "role-1", "secret-1", VaultOptions{Namespace: tc.ns, Egress: loopbackEgress})
			if err != nil {
				t.Fatalf("build client: %v", err)
			}
			if _, err := vc.Fetch("secret/data/x#value"); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if gotLogin != tc.want || gotRead != tc.want {
				t.Fatalf("namespace header = login:%q read:%q, want %q", gotLogin, gotRead, tc.want)
			}
		})
	}
}

// TestVaultRetryOnTransient (D4): a 503 on the read is retried until it succeeds.
func TestVaultRetryOnTransient(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/approle/login") {
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-abc", "lease_duration": 3600}})
			return
		}
		if reads.Add(1) < 3 { // fail the first two, succeed on the third
			http.Error(w, `{"errors":["standby"]}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "ok"}}})
	}))
	defer srv.Close()
	vc := loopbackVaultClient(srv.URL, "role-1", "secret-1")
	got, err := vc.Fetch("secret/data/x#value")
	if err != nil {
		t.Fatalf("fetch after retries: %v", err)
	}
	if got != "ok" {
		t.Fatalf("fetch = %q, want ok", got)
	}
	if n := reads.Load(); n != 3 {
		t.Fatalf("expected 3 read attempts (2 transient + success), got %d", n)
	}
}

// TestVaultBadStatusNotRetried: a 4xx (bad path/creds) is terminal, not retried.
func TestVaultBadStatusNotRetried(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/approle/login") {
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-abc", "lease_duration": 3600}})
			return
		}
		reads.Add(1)
		http.Error(w, `{"errors":["not found"]}`, http.StatusNotFound)
	}))
	defer srv.Close()
	vc := loopbackVaultClient(srv.URL, "role-1", "secret-1")
	if _, err := vc.Fetch("secret/data/x#value"); err == nil {
		t.Fatal("expected error on 404")
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("4xx must not retry: got %d read attempts", n)
	}
}

// TestVaultWrappedSecretID (D4): a response-wrapping token is unwrapped once via
// sys/wrapping/unwrap, the real secret_id is used for AppRole login, and the
// unwrap is NOT repeated on subsequent fetches (single-use token, cached).
func TestVaultWrappedSecretID(t *testing.T) {
	var unwraps, logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sys/wrapping/unwrap"):
			if r.Header.Get("X-Vault-Token") != "wrap-token" {
				http.Error(w, `{"errors":["bad wrapping token"]}`, http.StatusBadRequest)
				return
			}
			atomic.AddInt32(&unwraps, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"secret_id": "real-secret"}})
		case strings.HasSuffix(r.URL.Path, "/auth/approle/login"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["secret_id"] != "real-secret" {
				http.Error(w, `{"errors":["invalid secret id"]}`, http.StatusBadRequest)
				return
			}
			atomic.AddInt32(&logins, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-abc", "lease_duration": 3600}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "ok"}}})
		}
	}))
	defer srv.Close()
	vc, err := NewVaultClientWithOptions(srv.URL, "role-1", "wrap-token", VaultOptions{SecretIDWrapped: true, Egress: loopbackEgress})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	for i := range 2 {
		if got, err := vc.Fetch("secret/data/x#value"); err != nil || got != "ok" {
			t.Fatalf("fetch %d = %q, %v", i, got, err)
		}
	}
	if n := atomic.LoadInt32(&unwraps); n != 1 {
		t.Fatalf("expected exactly one unwrap (cached thereafter), got %d", n)
	}
	if n := atomic.LoadInt32(&logins); n != 1 {
		t.Fatalf("expected one login (token cached), got %d", n)
	}
}

// TestVaultCAFileInvalid (D4): a configured-but-unreadable/invalid CA bundle is a
// fail-loud construction error, never a silent fall-back to system roots.
func TestVaultCAFileInvalid(t *testing.T) {
	if _, err := NewVaultClientWithOptions("https://vault:8200", "r", "s", VaultOptions{CAFile: "/no/such/ca.pem"}); err == nil {
		t.Fatal("expected error for missing CA file")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVaultClientWithOptions("https://vault:8200", "r", "s", VaultOptions{CAFile: bad}); err == nil {
		t.Fatal("expected error for CA file with no valid PEM certs")
	}
}

// TestServiceRevealVaultSource is the gap the original tests missed: revealing a
// vault-source secret must go through Service.Reveal → VaultClient.Fetch, not
// just the client in isolation. With a real client wired, reveal returns the
// stored value; with the default stub it returns unavailable.
func TestServiceRevealVaultSource(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "rev.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Create a vault-source secret (stores the vault_ref, no ciphertext).
	sec, err := svc.Create(context.Background(),
		CreateInput{Key: "DB_PASSWORD", Source: "vault", VaultPath: "secret/data/cronomicon/app#DB_PASSWORD"}, "tester")
	if err != nil {
		t.Fatalf("create vault secret: %v", err)
	}

	// Default stub ⇒ unavailable (decision #8, no behavior change).
	if _, err := svc.Reveal(context.Background(), sec.ID); err == nil {
		t.Fatal("expected unavailable with stub vault client")
	}

	// Wire a real client against a fake Vault and seed the value.
	srv, store := fakeVault(t)
	store["secret/data/cronomicon/app"] = map[string]string{"DB_PASSWORD": "s3cr3t"}
	svc.WithVaultClient(loopbackVaultClient(srv.URL, "role-1", "secret-1"))

	got, err := svc.Reveal(context.Background(), sec.ID)
	if err != nil {
		t.Fatalf("reveal vault secret: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("reveal = %q, want s3cr3t", got)
	}
}

// TestVaultTokenAuth: with AuthMethod "token" the client presents the supplied
// token directly and never calls auth/approle/login — the login endpoint here
// fails the test if it is reached.
func TestVaultTokenAuth(t *testing.T) {
	var loginCalls atomic.Int32
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/approle/login") {
			loginCalls.Add(1)
			http.Error(w, `{"errors":["approle auth not enabled"]}`, http.StatusBadRequest)
			return
		}
		gotToken = r.Header.Get("X-Vault-Token")
		if gotToken != "hvs.operator-token" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "v"}}})
	}))
	defer srv.Close()

	// roleID is irrelevant under token auth; pass an empty one, as the settings
	// layer does when no AppRole was ever configured.
	vc, err := NewVaultClientWithOptions(srv.URL, "", "hvs.operator-token",
		VaultOptions{AuthMethod: "token", Egress: loopbackEgress})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	got, err := vc.Fetch("secret/data/x#value")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != "v" {
		t.Fatalf("fetch = %q, want v", got)
	}
	if n := loginCalls.Load(); n != 0 {
		t.Fatalf("approle login called %d times under token auth, want 0", n)
	}
	if gotToken != "hvs.operator-token" {
		t.Fatalf("X-Vault-Token = %q, want the configured token", gotToken)
	}
}

// TestVaultTokenAuthEmptyToken: token auth with no token fails loud rather than
// issuing an unauthenticated request that would 403 with a confusing message.
func TestVaultTokenAuthEmptyToken(t *testing.T) {
	vc, err := NewVaultClientWithOptions("https://vault.example:8200", "", "",
		VaultOptions{AuthMethod: "token", Egress: loopbackEgress})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if _, err := vc.Fetch("secret/data/x#value"); err == nil {
		t.Fatal("expected an error with token auth and no token")
	}
}

// TestVaultTokenAuthRejectsWrapped: response wrapping unwraps to an AppRole
// secret_id, so pairing it with token auth is a misconfiguration the constructor
// refuses (leaving the caller on the stub) rather than silently sending the
// single-use wrapping token as X-Vault-Token.
func TestVaultTokenAuthRejectsWrapped(t *testing.T) {
	if _, err := NewVaultClientWithOptions("https://vault.example:8200", "", "wrap-token",
		VaultOptions{AuthMethod: "token", SecretIDWrapped: true}); err == nil {
		t.Fatal("expected an error for token auth + wrapped secret_id")
	}
}
