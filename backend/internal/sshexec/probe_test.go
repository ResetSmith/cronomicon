package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"golang.org/x/crypto/ssh"
)

// probeFixture is a migrated DB + a probe-capable Service.
func probeFixture(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), t.TempDir())
	return svc, pool
}

func newKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, string(pem.EncodeToMemory(blk))
}

func storeKey(t *testing.T, pool *sql.DB, name, pemBody string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, value, created_at) VALUES(?, ?, ?, ?)`,
		db.NewID(), name, pemBody, now); err != nil {
		t.Fatal(err)
	}
}

func insertHost(t *testing.T, pool *sql.DB, id, addr string, port int, user, keyName, hostKey, via string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, via, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "host-"+id, addr, port, user, keyName, nullable(hostKey), nullable(via), now); err != nil {
		t.Fatal(err)
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestProbeHost_Verified(t *testing.T) {
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "GOOD_KEY", clientPEM)
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "GOOD_KEY",
		string(ssh.MarshalAuthorizedKey(hostKey)), "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil {
		t.Fatalf("ProbeHost err: %v", err)
	}
	if res.Status != StatusVerified {
		t.Fatalf("status = %q (%s), want verified", res.Status, res.Message)
	}
	if res.CheckedAt == "" {
		t.Error("CheckedAt empty")
	}
}

func TestProbeHost_VerifiedTOFU(t *testing.T) {
	// No stored host_key ⇒ TOFU capture on first connect, still verified, and
	// the captured key is persisted.
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "GOOD_KEY", clientPEM)
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "GOOD_KEY", "", "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil || res.Status != StatusVerified {
		t.Fatalf("status = %q (%s) err=%v, want verified", res.Status, res.Message, err)
	}
	var captured string
	_ = pool.QueryRow(`SELECT COALESCE(host_key,'') FROM ssh_hosts WHERE id='h1'`).Scan(&captured)
	if captured == "" {
		t.Error("expected TOFU to capture and persist the host key")
	}
}

func TestProbeHost_CredError_WrongKey(t *testing.T) {
	svc, pool := probeFixture(t)
	serverClient, _ := newKey(t) // the key the server will accept
	_, wrongPEM := newKey(t)     // a different key we configure on the host
	addr, hostKey := testSSHServer(t, serverClient.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "WRONG_KEY", wrongPEM)
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "WRONG_KEY",
		string(ssh.MarshalAuthorizedKey(hostKey)), "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error", res.Status, res.Message)
	}
}

func TestProbeHost_CredError_MissingKey(t *testing.T) {
	svc, pool := probeFixture(t)
	// auth_key_env_var points to a key that isn't stored anywhere.
	insertHost(t, pool, "h1", "127.0.0.1", 22, "tester", "NOPE", "", "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error (no dial should be attempted)", res.Status, res.Message)
	}
}

func TestProbeHost_CredError_GarbageKey(t *testing.T) {
	svc, pool := probeFixture(t)
	storeKey(t, pool, "BAD_PEM", "-----BEGIN OPENSSH PRIVATE KEY-----\nnot a key\n-----END OPENSSH PRIVATE KEY-----")
	insertHost(t, pool, "h1", "127.0.0.1", 22, "tester", "BAD_PEM", "", "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error", res.Status, res.Message)
	}
}

func TestProbeHost_CredError_EncryptedKey(t *testing.T) {
	// A passphrase-protected private key resolves fine from the secret store but
	// x/crypto/ssh refuses to parse it ⇒ cred_error with the passphrase hint.
	svc, pool := probeFixture(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	storeKey(t, pool, "ENC_KEY", string(pem.EncodeToMemory(blk)))
	insertHost(t, pool, "h1", "127.0.0.1", 22, "tester", "ENC_KEY", "", "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "passphrase") {
		t.Errorf("message = %q, want it to mention the passphrase cause", res.Message)
	}
}

func TestProbeHost_ConnError_Refused(t *testing.T) {
	svc, pool := probeFixture(t)
	_, clientPEM := newKey(t)
	// Bind then close to obtain a port nothing is listening on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "GOOD_KEY", clientPEM)
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "GOOD_KEY", "", "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusConnError {
		t.Fatalf("status = %q (%s), want conn_error", res.Status, res.Message)
	}
}

func TestProbeHost_ConnError_HostKeyMismatch(t *testing.T) {
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	// Pin a DIFFERENT host key than the server actually presents.
	otherSigner, _ := newKey(t)
	storeKey(t, pool, "GOOD_KEY", clientPEM)
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "GOOD_KEY",
		string(ssh.MarshalAuthorizedKey(otherSigner.PublicKey())), "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusConnError {
		t.Fatalf("status = %q (%s), want conn_error", res.Status, res.Message)
	}
}

func TestProbeHost_ConnError_DeadBastion(t *testing.T) {
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "GOOD_KEY", clientPEM)
	// via names a bastion that doesn't exist ⇒ resolution fails before the host hop.
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "GOOD_KEY",
		string(ssh.MarshalAuthorizedKey(hostKey)), "ghost-bastion")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusConnError {
		t.Fatalf("status = %q (%s), want conn_error", res.Status, res.Message)
	}
}

// ── Reachability tier (TT): keyless targets ──────────────────────────────────

func TestProbeHost_Reachable_KeylessTOFU(t *testing.T) {
	// A target with no credential and no key name gets the unauthenticated tier:
	// the endpoint answers, the host key is TOFU-captured and persisted, and the
	// probe reports reachable — never cred_error.
	svc, pool := probeFixture(t)
	client, _ := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	insertHost(t, pool, "h1", host, atoiPort(port), "", "", "", "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil {
		t.Fatalf("ProbeHost err: %v", err)
	}
	if res.Status != StatusReachable {
		t.Fatalf("status = %q (%s), want reachable", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "authentication was not tested") {
		t.Errorf("message = %q, want it to state auth was not tested", res.Message)
	}
	var captured string
	_ = pool.QueryRow(`SELECT COALESCE(host_key,'') FROM ssh_hosts WHERE id='h1'`).Scan(&captured)
	if captured == "" {
		t.Error("expected the reachability tier to TOFU-capture and persist the host key")
	}
}

func TestProbeHost_Reachable_PinnedMatch(t *testing.T) {
	// Keyless target with a correctly pinned host key: still reachable — the
	// strict compare ran and passed.
	svc, pool := probeFixture(t)
	client, _ := newKey(t)
	addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	insertHost(t, pool, "h1", host, atoiPort(port), "", "",
		string(ssh.MarshalAuthorizedKey(hostKey)), "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusReachable {
		t.Fatalf("status = %q (%s), want reachable", res.Status, res.Message)
	}
}

func TestProbeHost_Reachable_PinnedMismatch(t *testing.T) {
	// The keyless tier still enforces the pin: a changed host key is conn_error
	// (possible MITM), not reachable.
	svc, pool := probeFixture(t)
	client, _ := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	otherSigner, _ := newKey(t)
	insertHost(t, pool, "h1", host, atoiPort(port), "", "",
		string(ssh.MarshalAuthorizedKey(otherSigner.PublicKey())), "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusConnError {
		t.Fatalf("status = %q (%s), want conn_error", res.Status, res.Message)
	}
}

func TestProbeHost_Reachable_ConnRefused(t *testing.T) {
	svc, pool := probeFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, port, _ := net.SplitHostPort(addr)

	insertHost(t, pool, "h1", host, atoiPort(port), "", "", "", "")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusConnError {
		t.Fatalf("status = %q (%s), want conn_error", res.Status, res.Message)
	}
}

func TestProbeHost_Reachable_KeylessBastionHop(t *testing.T) {
	// A keyless target behind a KEYLESS bastion cannot be probed: the hop must
	// authenticate and there is no target signer to fall back to. cred_error
	// naming the bastion, before any network I/O.
	svc, pool := probeFixture(t)
	insertBastion(t, svc, "b1", "jump", "")
	insertHost(t, pool, "h1", "127.0.0.1", 22, "", "", "", "jump")

	res, _ := svc.ProbeHost(context.Background(), "h1")
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "jump") {
		t.Errorf("message = %q, want it to name the keyless bastion", res.Message)
	}
}

func TestProbeBastion_Reachable_Keyless(t *testing.T) {
	// A keyless bastion gets the same tier: reachable + its host key pinned.
	svc, pool := probeFixture(t)
	client, _ := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO bastions(id, hostname, name, address, port, username, created_at)
		VALUES('b1', ?, 'jump', ?, ?, 'jump', ?)`,
		host, host, atoiPort(port), now); err != nil {
		t.Fatal(err)
	}

	res, err := svc.ProbeBastion(context.Background(), "b1")
	if err != nil {
		t.Fatalf("ProbeBastion err: %v", err)
	}
	if res.Status != StatusReachable {
		t.Fatalf("status = %q (%s), want reachable", res.Status, res.Message)
	}
	var captured string
	_ = pool.QueryRow(`SELECT COALESCE(host_key,'') FROM bastions WHERE id='b1'`).Scan(&captured)
	if captured == "" {
		t.Error("expected the reachability tier to TOFU-capture the bastion host key")
	}
}

func TestProbeHost_NotFound(t *testing.T) {
	svc, _ := probeFixture(t)
	if _, err := svc.ProbeHost(context.Background(), "nope"); err != ErrProbeNotFound {
		t.Fatalf("err = %v, want ErrProbeNotFound", err)
	}
}

func TestProbeHost_InFlight(t *testing.T) {
	svc, pool := probeFixture(t)
	insertHost(t, pool, "h1", "127.0.0.1", 22, "tester", "NOPE", "", "")
	// Hold the per-target lock, then a probe for the same id must report in-flight.
	release, ok := svc.probeAcquire("host:h1")
	if !ok {
		t.Fatal("could not acquire probe lock")
	}
	defer release()
	if _, err := svc.ProbeHost(context.Background(), "h1"); err != ErrProbeInFlight {
		t.Fatalf("err = %v, want ErrProbeInFlight", err)
	}
}

func TestProbeBastion_Verified_UsesOwnKey(t *testing.T) {
	// Regression for the TC.2 signer subtlety: the bastion probe must auth with
	// the bastion's OWN key/user, not a host's.
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "BASTION_KEY", clientPEM)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO bastions(id, hostname, name, address, port, username, auth_key_env_var, created_at)
		VALUES('b1', ?, 'jump', ?, ?, 'tester', 'BASTION_KEY', ?)`,
		host, host, atoiPort(port), now); err != nil {
		t.Fatal(err)
	}

	res, err := svc.ProbeBastion(context.Background(), "b1")
	if err != nil {
		t.Fatalf("ProbeBastion err: %v", err)
	}
	if res.Status != StatusVerified {
		t.Fatalf("status = %q (%s), want verified", res.Status, res.Message)
	}
}

func TestProbeBastion_CredError(t *testing.T) {
	svc, pool := probeFixture(t)
	serverClient, _ := newKey(t)
	_, wrongPEM := newKey(t)
	addr, _ := testSSHServer(t, serverClient.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "BASTION_KEY", wrongPEM)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO bastions(id, hostname, name, address, port, username, auth_key_env_var, created_at)
		VALUES('b1', ?, 'jump', ?, ?, 'tester', 'BASTION_KEY', ?)`,
		host, host, atoiPort(port), now); err != nil {
		t.Fatal(err)
	}

	res, _ := svc.ProbeBastion(context.Background(), "b1")
	if res.Status != StatusCredError {
		t.Fatalf("status = %q (%s), want cred_error", res.Status, res.Message)
	}
}

func TestProbeBastion_NotFound(t *testing.T) {
	svc, _ := probeFixture(t)
	if _, err := svc.ProbeBastion(context.Background(), "nope"); err != ErrProbeNotFound {
		t.Fatalf("err = %v, want ErrProbeNotFound", err)
	}
}
