package sshkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"golang.org/x/crypto/ssh"
)

// testCfg returns a config with a 32-byte base64 KEK (the fixture the sshexec
// tests use), shared by the service and backfill tests.
func testCfg() *config.Config {
	return &config.Config{SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
}

func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "sshkeys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	return New(pool, testCfg(), slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

// TestService_StoredRoundTrip covers create (validate-on-save + seal) → Get
// (metadata only) → Resolve (open + parse to a usable signer) → Usage + the
// delete guard (blocked in use, force orphans the reference).
func TestService_StoredRoundTrip(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()

	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	material := pkcs8PEM(t, ed)

	cred, err := svc.Create(ctx, CreateInput{Label: "prod_deploy", Source: "stored", Material: material}, "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cred.KeyType == nil || *cred.KeyType != "ssh-ed25519" {
		t.Errorf("keyType = %v, want ssh-ed25519", cred.KeyType)
	}
	if cred.Fingerprint == nil || !strings.HasPrefix(*cred.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %v, want SHA256: prefix", cred.Fingerprint)
	}
	if cred.PublicKey == nil {
		t.Fatal("public key not derived on save")
	}

	// resolveSigner decrypts and yields a signer whose public key matches what we stored.
	signer, err := resolveSigner(ctx, svc.db, svc.sealer, nil, cred.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	gotPub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if gotPub != *cred.PublicKey {
		t.Errorf("resolved pub %q != stored %q", gotPub, *cred.PublicKey)
	}

	// Reference the credential from a host; usage reports it and delete is guarded.
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, port, created_at, source, auth_credential_id)
	      VALUES('h1','web1.example.com',22,'t','amadeus',?)`, cred.ID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	u, err := svc.Usage(ctx, cred.ID)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(u.Hosts) != 1 || u.Hosts[0].Name != "web1.example.com" {
		t.Errorf("usage hosts = %+v, want one (web1.example.com)", u.Hosts)
	}
	if _, err := svc.Delete(ctx, cred.ID, false); !errors.Is(err, ErrCredentialInUse) {
		t.Fatalf("delete(force=false) = %v, want ErrCredentialInUse", err)
	}

	// Force delete orphans the host reference and removes the credential.
	ok, err := svc.Delete(ctx, cred.ID, true)
	if err != nil || !ok {
		t.Fatalf("force delete: ok=%v err=%v", ok, err)
	}
	var orphaned int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts WHERE id='h1' AND auth_credential_id IS NULL`).Scan(&orphaned)
	if orphaned != 1 {
		t.Errorf("host reference not orphaned after force delete")
	}
	if got, _ := svc.Get(ctx, cred.ID); got != nil {
		t.Errorf("credential still present after force delete")
	}
}

// TestService_RejectsBadKeyOnSave proves validation happens at save time, not at
// dial time.
func TestService_RejectsBadKeyOnSave(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Create(context.Background(),
		CreateInput{Label: "bad", Source: "stored", Material: "not a key"}, "tester"); err == nil {
		t.Fatal("create with unparseable material should fail validation-on-save")
	}
}

// TestService_RotatePreservesID proves rotating material keeps the id (so host/
// bastion FK references follow) while changing the fingerprint.
func TestService_RotatePreservesID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, ed1, _ := ed25519.GenerateKey(rand.Reader)
	cred, err := svc.Create(ctx, CreateInput{Label: "k", Source: "stored", Material: pkcs8PEM(t, ed1)}, "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, ed2, _ := ed25519.GenerateKey(rand.Reader)
	rotated, err := svc.Update(ctx, cred.ID, UpdateInput{Label: "k", Source: "stored", Material: pkcs8PEM(t, ed2)}, "tester")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.ID != cred.ID {
		t.Errorf("id changed on rotate: %q → %q", cred.ID, rotated.ID)
	}
	if *rotated.Fingerprint == *cred.Fingerprint {
		t.Errorf("fingerprint unchanged after rotating to a new key")
	}
}
