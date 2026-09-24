package sshkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"golang.org/x/crypto/ssh"
)

// fakeVault is an in-memory secrets.VaultClient (vault_ref → material). It stands
// in for the app's configured Vault client so the P2.4 resolution path can be
// exercised without a live Vault.
type fakeVault map[string]string

func (f fakeVault) Fetch(ref string) (string, error) {
	if v, ok := f[ref]; ok {
		return v, nil
	}
	return "", errors.New("vault: no value at ref")
}
func (f fakeVault) Write(ref, val string) error { f[ref] = val; return nil }

// seedVaultCred inserts a vault-source ssh_credentials row (no envelope columns,
// only a vault_ref) directly, mirroring what a vault-backed key looks like on disk.
func seedVaultCred(t *testing.T, pool *sql.DB, label, vaultRef string) string {
	t.Helper()
	id := db.NewID()
	if _, err := pool.Exec(
		`INSERT INTO ssh_credentials (id, label, source, vault_ref, created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, ?, 'vault', ?, 'tester', '2026-01-01T00:00:00Z', 'tester', '2026-01-01T00:00:00Z')`,
		id, label, vaultRef); err != nil {
		t.Fatalf("insert vault ssh credential: %v", err)
	}
	return id
}

// TestResolveVaultSourceCredential (P2.4): a vault-source SSH credential resolves
// its private key live through the configured Vault client — as raw material
// (ResolveMaterialByName, the runner key-delivery path) and as a parsed signer
// (ResolveSigner, the SSH-executor connection path).
func TestResolveVaultSourceCredential(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cfg := testCfg()

	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	material := pkcs8PEM(t, ed)
	const ref = "secret/data/ssh/deploy#private_key"
	vault := fakeVault{ref: material}
	seedVaultCred(t, pool, "vault_deploy", ref)

	// Raw material (runner path): fetched verbatim from Vault.
	got, found, err := ResolveMaterialByName(ctx, pool, cfg, vault, "vault_deploy")
	if err != nil || !found {
		t.Fatalf("ResolveMaterialByName vault: found=%v err=%v", found, err)
	}
	if got != material {
		t.Fatalf("vault material mismatch")
	}

	// Parsed signer (SSH-executor path): the fetched PEM parses to a usable signer.
	signer, err := ResolveSigner(ctx, pool, cfg, vault, mustCredID(t, pool, "vault_deploy"))
	if err != nil {
		t.Fatalf("ResolveSigner vault: %v", err)
	}
	if !strings.HasPrefix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "ssh-ed25519") {
		t.Fatalf("unexpected resolved key type")
	}
	_ = svc
}

// TestResolveVaultSourceFailsClosed: with no Vault client wired (nil / stub), a
// vault-source credential is unusable — an error, never a silent empty key.
func TestResolveVaultSourceFailsClosed(t *testing.T) {
	_, pool := newTestService(t)
	ctx := context.Background()
	cfg := testCfg()
	seedVaultCred(t, pool, "vault_deploy", "secret/data/ssh/deploy#private_key")

	if _, _, err := ResolveMaterialByName(ctx, pool, cfg, nil, "vault_deploy"); err == nil {
		t.Fatal("expected fail-closed error with no Vault client")
	}
	// A reachable-but-empty ref also fails closed rather than returning "".
	if _, _, err := ResolveMaterialByName(ctx, pool, cfg, fakeVault{}, "vault_deploy"); err == nil {
		t.Fatal("expected error when Vault has no value at the ref")
	}
}

func mustCredID(t *testing.T, pool *sql.DB, label string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(`SELECT id FROM ssh_credentials WHERE label = ?`, label).Scan(&id); err != nil {
		t.Fatalf("lookup cred id: %v", err)
	}
	return id
}

var _ secrets.VaultClient = fakeVault{}
