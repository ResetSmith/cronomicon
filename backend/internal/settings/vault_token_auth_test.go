package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// vaultTestConfig returns a config with a KEK (the credentials are encrypted at
// rest) and an egress posture that lets the status probe dial an httptest server.
func vaultTestConfig(t *testing.T) *config.Config {
	t.Helper()
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return &config.Config{
		SecretKEKEnv:          base64.StdEncoding.EncodeToString(kek),
		OutboundAllowPrivate:  true,
		OutboundAllowLoopback: true,
	}
}

// healthyVault serves the unauthenticated sys/health the status badge probes.
func healthyVault(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestVaultConfigTokenAuthSave is the regression for the 500 on the first save
// under token auth: role_id is NOT NULL, token auth supplies no role ID, and
// binding it as NULL failed the constraint ("upsert vault_config: NOT NULL
// constraint failed: vault_config.role_id") before any Vault config existed.
func TestVaultConfigTokenAuthSave(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := vaultTestConfig(t)
	addr := healthyVault(t)

	got, err := UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr:       addr,
		AuthMethod: "token",
		SecretId:   "hvs.operator-token",
	}, "tester")
	if err != nil {
		t.Fatalf("save token auth on a fresh config: %v", err)
	}
	if got.AuthMethod != "token" {
		t.Fatalf("authMethod = %q, want token", got.AuthMethod)
	}
	if !got.SecretIdSet || got.SecretId != "" {
		t.Fatalf("token should be stored and never returned: %+v", got)
	}
	if got.RoleIdSet {
		t.Fatalf("token auth stores no role ID: %+v", got)
	}
	// A token alone is complete credentials, so the badge must not read
	// "unconfigured" the way it would for a half-filled AppRole.
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}

	// And the runtime resolves a token-auth client, not a stub.
	rt, ok := ResolveVaultRuntime(ctx, pool, cfg)
	if !ok {
		t.Fatal("ResolveVaultRuntime: not ok under token auth")
	}
	if rt.AuthMethod != "token" || rt.SecretID != "hvs.operator-token" || rt.RoleID != "" {
		t.Fatalf("runtime = %+v, want token auth with the stored token and no role", rt)
	}
}

// TestVaultConfigTokenAuthResave: a second save with the credential left blank
// keeps the stored token (the "leave blank to keep" contract) — the round trip
// an operator makes when editing only the address or namespace.
func TestVaultConfigTokenAuthResave(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := vaultTestConfig(t)
	addr := healthyVault(t)

	if _, err := UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr: addr, AuthMethod: "token", SecretId: "hvs.operator-token",
	}, "tester"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	ns := "team-a"
	got, err := UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr: addr, AuthMethod: "token", Namespace: &ns,
	}, "tester")
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !got.SecretIdSet {
		t.Fatal("blank credential on re-save dropped the stored token")
	}
	rt, ok := ResolveVaultRuntime(ctx, pool, cfg)
	if !ok || rt.SecretID != "hvs.operator-token" {
		t.Fatalf("runtime = %+v (ok=%v), want the preserved token", rt, ok)
	}
}

// TestVaultConfigAuthMethodSwitchClearsCredential: an AppRole secret_id is not a
// Vault token. Switching methods without supplying a new credential must clear
// it rather than carry it over into a client that would 403 on every read.
func TestVaultConfigAuthMethodSwitchClearsCredential(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := vaultTestConfig(t)
	addr := healthyVault(t)

	if _, err := UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr: addr, AuthMethod: "approle", RoleId: "role-1", SecretId: "secret-1",
	}, "tester"); err != nil {
		t.Fatalf("approle save: %v", err)
	}

	got, err := UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr: addr, AuthMethod: "token", RoleId: "role-1",
	}, "tester")
	if err != nil {
		t.Fatalf("switch to token: %v", err)
	}
	if got.SecretIdSet {
		t.Fatal("the AppRole secret_id survived the switch to token auth")
	}
	if got.Status != "unconfigured" {
		t.Fatalf("status = %q, want unconfigured until a token is supplied", got.Status)
	}
	if _, ok := ResolveVaultRuntime(ctx, pool, cfg); ok {
		t.Fatal("ResolveVaultRuntime wired a client with no token")
	}

	// Supplying the token completes it.
	got, err = UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr: addr, AuthMethod: "token", SecretId: "hvs.operator-token",
	}, "tester")
	if err != nil {
		t.Fatalf("supply token: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}
}

// TestVaultConfigAppRoleIncomplete: the AppRole path still demands both halves,
// so the token-auth relaxation cannot leak into it.
func TestVaultConfigAppRoleIncomplete(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := vaultTestConfig(t)
	addr := healthyVault(t)

	got, err := UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr: addr, AuthMethod: "approle", SecretId: "secret-1",
	}, "tester")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got.Status != "unconfigured" {
		t.Fatalf("status = %q, want unconfigured with no role ID", got.Status)
	}
	if _, ok := ResolveVaultRuntime(ctx, pool, cfg); ok {
		t.Fatal("ResolveVaultRuntime wired an AppRole client with no role ID")
	}
}
