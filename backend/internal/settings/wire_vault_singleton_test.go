package settings_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// TestWireVaultClientSharesWrappedSecretID is the H4 regression: two subsystems
// each call WireVaultClient with the same resolved config (as the API server, SSH
// executor, runner, and backfill all do at startup) and reveal a vault-source
// secret. Before the fix each built its own httpVaultClient, and with a
// response-wrapped secret_id the single-use wrapping token unwrapped once — the
// losing client then 400'd forever. The process-singleton cache makes both share
// ONE client and ONE unwrap.
func TestWireVaultClientSharesWrappedSecretID(t *testing.T) {
	var unwraps, logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sys/wrapping/unwrap"):
			if r.Header.Get("X-Vault-Token") != "wrap-token" {
				http.Error(w, `{"errors":["bad or used wrapping token"]}`, http.StatusBadRequest)
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
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "the-secret"}}})
		}
	}))
	defer srv.Close()

	pool, err := db.Open(filepath.Join(t.TempDir(), "wire.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Env-based AppRole with a WRAPPED secret_id, pointed at the mock Vault. The
	// distinct httptest addr per test also keeps the process-wide client cache from
	// bleeding into other tests.
	cfg := &config.Config{
		VaultAddr:            srv.URL,
		VaultRoleID:          "role-1",
		VaultSecretID:        "wrap-token",
		VaultSecretIDWrapped: true,
		// The fake Vault is a loopback httptest server; allow loopback past the SU-7 guard.
		OutboundAllowPrivate:  true,
		OutboundAllowLoopback: true,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Seed a vault-source secret (stores vault_ref, no ciphertext — needs no KEK).
	seed := secrets.New(pool, cfg, log)
	sc, err := seed.Create(context.Background(),
		secrets.CreateInput{Key: "DB_PASSWORD", Source: "vault", VaultPath: "secret/data/x#value"}, "seed")
	if err != nil {
		t.Fatalf("create vault secret: %v", err)
	}

	// Two independent services, each wired the way a separate constructor would.
	sec1 := settings.WireVaultClient(context.Background(), pool, cfg, secrets.New(pool, cfg, log), log)
	sec2 := settings.WireVaultClient(context.Background(), pool, cfg, secrets.New(pool, cfg, log), log)

	if v, err := sec1.Reveal(context.Background(), sc.ID); err != nil || v != "the-secret" {
		t.Fatalf("sec1 reveal = %q, %v; want the-secret", v, err)
	}
	// The second service — a DISTINCT httpVaultClient before H4 — would unwrap the
	// already-consumed wrapping token again and 400. Sharing the client, it reuses
	// the cached token.
	if v, err := sec2.Reveal(context.Background(), sc.ID); err != nil || v != "the-secret" {
		t.Fatalf("sec2 reveal = %q, %v; want the-secret", v, err)
	}

	if n := atomic.LoadInt32(&unwraps); n != 1 {
		t.Errorf("expected exactly ONE unwrap across both wired services, got %d", n)
	}
	if n := atomic.LoadInt32(&logins); n != 1 {
		t.Errorf("expected ONE AppRole login (shared cached token), got %d", n)
	}
}
