package sshexec

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"golang.org/x/crypto/ssh"
)

// createCred seals pemBody into a stored ssh_credentials row via the real service
// (validate-on-save + envelope-seal) and returns its id.
func createCred(t *testing.T, svc *Service, label, pemBody string) string {
	t.Helper()
	c, err := sshkeys.New(svc.db, svc.cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).
		Create(context.Background(), sshkeys.CreateInput{Label: label, Source: "stored", Material: pemBody}, "test")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	return c.ID
}

// insertHostCred inserts an ssh_hosts row that references a credential by FK
// (auth_credential_id) rather than the legacy name.
func insertHostCred(t *testing.T, pool *sql.DB, id, addr string, port int, user, credID, hostKey, via string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_credential_id, host_key, via, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "host-"+id, addr, port, user, credID, nullable(hostKey), nullable(via), now); err != nil {
		t.Fatal(err)
	}
}

// TestProbeHost_Verified_Credential (SK.12) proves the credential path: a host
// referencing a first-class credential authenticates, and the verified message
// names the key by fingerprint so the operator knows WHICH key authenticated.
func TestProbeHost_Verified_Credential(t *testing.T) {
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	credID := createCred(t, svc, "deploy_key", clientPEM)
	insertHostCred(t, pool, "h1", host, atoiPort(port), "tester", credID,
		string(ssh.MarshalAuthorizedKey(hostKey)), "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil {
		t.Fatalf("ProbeHost err: %v", err)
	}
	if res.Status != StatusVerified {
		t.Fatalf("status = %q (%s), want verified", res.Status, res.Message)
	}
	if fp := ssh.FingerprintSHA256(client.PublicKey()); !strings.Contains(res.Message, fp) {
		t.Errorf("verified message %q does not name the key fingerprint %q", res.Message, fp)
	}
}

// TestProbeHost_CredError_UndecryptableCredential (SK.12) proves a host wired to a
// credential whose material won't decrypt (e.g. corruption / KEK rotated away)
// classifies as cred_error before any dial. (A host can't reference a *missing*
// credential — the FK forbids it — so undecryptable is the realistic failure.)
func TestProbeHost_CredError_UndecryptableCredential(t *testing.T) {
	svc, pool := probeFixture(t)
	if _, err := pool.Exec(`INSERT INTO ssh_credentials(id, label, source, ciphertext, nonce, wrapped_dek, kek_version, created_at)
	      VALUES('badcred', 'badcred', 'stored', X'00', X'00', X'00', 0, 't')`); err != nil {
		t.Fatal(err)
	}
	insertHostCred(t, pool, "h1", "127.0.0.1", 2222, "tester", "badcred", "", "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil {
		t.Fatalf("ProbeHost err: %v", err)
	}
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error", res.Status, res.Message)
	}
}

// TestLoadSigner_DualRead (SK.15) covers the resolution precedence directly: a
// credential id wins and is fatal on failure (no silent fallthrough to the name
// path, PP-M7); the legacy name path resolves only when no credential is set.
func TestLoadSigner_DualRead(t *testing.T) {
	svc, pool := probeFixture(t)
	ctx := context.Background()

	credSigner, credPEM := newKey(t)
	_, namePEM := newKey(t)
	credID := createCred(t, svc, "cred", credPEM)
	storeKey(t, pool, "NAME_KEY", namePEM) // plaintext env var

	// 1. Credential id present → resolves to the credential's key.
	got, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, credID, "")
	if err != nil {
		t.Fatalf("credential resolve: %v", err)
	}
	if string(got.PublicKey().Marshal()) != string(credSigner.PublicKey().Marshal()) {
		t.Error("resolved signer is not the credential's key")
	}

	// 2. Credential id present but missing → fatal, with NO fallthrough to the name
	//    path even though a valid NAME_KEY is also supplied.
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "does-not-exist", "NAME_KEY"); err == nil {
		t.Error("missing credential should be fatal (no fallthrough to the name path)")
	}

	// 3. No credential id → legacy name path resolves.
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "", "NAME_KEY"); err != nil {
		t.Errorf("name fallback should resolve: %v", err)
	}

	// 4. PP-M7: a found-but-undecryptable credential is fatal (no fallthrough).
	if _, err := pool.Exec(`INSERT INTO ssh_credentials(id, label, source, ciphertext, nonce, wrapped_dek, kek_version, created_at)
	      VALUES('bad', 'bad', 'stored', X'00', X'00', X'00', 0, 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "bad", "NAME_KEY"); err == nil {
		t.Error("undecryptable credential should be fatal (no fallthrough)")
	}

	// 5. Derived reference (W3): CRONOMICON_KEY_<label> routes to ssh_credentials by
	//    label — here to the same key the credID path resolves.
	got5, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "", "CRONOMICON_KEY_cred")
	if err != nil {
		t.Fatalf("CRONOMICON_KEY_cred resolve: %v", err)
	}
	if string(got5.PublicKey().Marshal()) != string(credSigner.PublicKey().Marshal()) {
		t.Error("CRONOMICON_KEY_cred did not resolve to the labelled credential's key")
	}

	// 6. CRONOMICON_VAR_<name> routes to the env_vars (Variables) table.
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "", "CRONOMICON_VAR_NAME_KEY"); err != nil {
		t.Errorf("CRONOMICON_VAR_NAME_KEY should resolve via env_vars: %v", err)
	}

	// 7. A prefixed reference that resolves to nothing is a HARD error — no fallback
	//    to the bare chain (deterministic routing).
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "", "CRONOMICON_KEY_nonexistent"); err == nil {
		t.Error("CRONOMICON_KEY_nonexistent should be fatal (no fallback)")
	}

	// 8. Section routing is exact: CRONOMICON_SECRET_NAME_KEY looks ONLY at the secrets
	//    table, so a name that lives in env_vars does not resolve.
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "", "CRONOMICON_SECRET_NAME_KEY"); err == nil {
		t.Error("CRONOMICON_SECRET_NAME_KEY must not fall back to env_vars (deterministic routing)")
	}
}
