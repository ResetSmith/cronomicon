package settings_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// TestVaultConfigChangeReachesWiredServices is the regression for the
// restart-required gap: the runner service, SSH executor, and API secrets
// service call WireVaultClient once at construction, so a vault_config edit
// (first-time setup, a rotated token) previously reached only the per-request
// paths (capabilities, run-detail redaction) — dispatch kept the stale client
// until a process restart, while the status badge probed the NEW config and
// read "ok". The dynamic client resolves the current config per operation, so
// one long-lived service must observe: unconfigured → configured → rotated,
// with no re-wiring.
func TestVaultConfigChangeReachesWiredServices(t *testing.T) {
	// A token-auth mock Vault that only accepts the CURRENT token, which the
	// test swaps to simulate an operator-side rotation.
	var wantToken atomic.Value
	wantToken.Store("hvs.token-one")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sys/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("X-Vault-Token") != wantToken.Load().(string) {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "the-secret"}}})
	}))
	defer srv.Close()

	pool, err := db.Open(filepath.Join(t.TempDir(), "dynamic.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	cfg := &config.Config{
		// The stored token is encrypted at rest, so the config needs a KEK; no
		// AMADEUS_VAULT_* env, so the DB-backed vault_config is the live source.
		SecretKEKEnv: base64.StdEncoding.EncodeToString(kek),
		// The mock Vault is a loopback httptest server; allow it past the SU-7 guard.
		OutboundAllowPrivate:  true,
		OutboundAllowLoopback: true,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// ONE long-lived service, wired once at "startup" like the executors are.
	sec := settings.WireVaultClient(ctx, pool, cfg, secrets.New(pool, cfg, log), log)
	sc, err := sec.Create(ctx, secrets.CreateInput{Key: "DB_PASSWORD", Source: "vault", VaultPath: "secret/data/x#value"}, "seed")
	if err != nil {
		t.Fatalf("create vault secret: %v", err)
	}

	// Unconfigured: the reveal fails "unavailable" and the L7 signal is false.
	if _, err := sec.Reveal(ctx, sc.ID); !errors.Is(err, secrets.ErrVaultUnavailable) {
		t.Fatalf("reveal before any vault config = %v, want ErrVaultUnavailable", err)
	}
	if sec.VaultConfigured() {
		t.Fatal("VaultConfigured = true before any vault config")
	}

	// First-time configuration via the settings write — NO re-wire, NO restart.
	if _, err := settings.UpdateVaultConfig(ctx, pool, cfg, settings.VaultConfig{
		Addr: srv.URL, AuthMethod: "token", SecretId: "hvs.token-one",
	}, "tester"); err != nil {
		t.Fatalf("configure vault: %v", err)
	}
	if v, err := sec.Reveal(ctx, sc.ID); err != nil || v != "the-secret" {
		t.Fatalf("reveal after configuring = %q, %v; want the-secret", v, err)
	}
	if !sec.VaultConfigured() {
		t.Fatal("VaultConfigured = false after configuring")
	}

	// Rotation: Vault-side the old token dies, operator stores the new one. The
	// same service must pick it up on its next operation.
	wantToken.Store("hvs.token-two")
	if _, err := sec.Reveal(ctx, sc.ID); err == nil {
		t.Fatal("reveal with a lapsed token unexpectedly succeeded")
	}
	if _, err := settings.UpdateVaultConfig(ctx, pool, cfg, settings.VaultConfig{
		Addr: srv.URL, AuthMethod: "token", SecretId: "hvs.token-two",
	}, "tester"); err != nil {
		t.Fatalf("store rotated token: %v", err)
	}
	if v, err := sec.Reveal(ctx, sc.ID); err != nil || v != "the-secret" {
		t.Fatalf("reveal after rotation = %q, %v; want the-secret", v, err)
	}
}
