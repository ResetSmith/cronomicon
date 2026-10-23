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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
	"github.com/ResetSmith/cronomicon/internal/settings"
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

// insertHost writes a host record. hostKey, when given (authorized-key form),
// is APPROVED for the local runner under the record's address — where a host's
// key lives since 2.3.0; it used to be a column on the record.
func insertHost(t *testing.T, pool *sql.DB, id, addr string, port int, user, keyName, hostKey, via string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, via, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "host-"+id, addr, port, user, keyName, nullable(via), now); err != nil {
		t.Fatal(err)
	}
	trustHostKeyLine(t, pool, addr, port, hostKey)
}

// trustHostKey approves key for the local runner under the address a record is
// dialled at: the row an operator's approval, or the upgrade's carry, leaves in
// the ledger. It creates the local runner's row if the test has not.
func trustHostKey(t *testing.T, pool *sql.DB, addr string, port int, key ssh.PublicKey) {
	t.Helper()
	id, _, err := settings.EnsureLocalRunner(context.Background(), pool, true, 4)
	if err != nil {
		t.Fatalf("the local runner's row: %v", err)
	}
	target := addr
	if port != 0 && port != 22 {
		target = net.JoinHostPort(addr, strconv.Itoa(port))
	}
	pattern := hostkeys.Pattern(target)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO host_key_ledger (batch_id, runner_id, runner_name, host, key_type, fingerprint, known_hosts_line,
		                             decision, source, actor, decided_at, delivered_at, confirmed_at)
		VALUES ('test', ?, 'Local runner', ?, ?, ?, ?, 'approved', 'pasted', 'test', ?, ?, ?)`,
		id, pattern, key.Type(), ssh.FingerprintSHA256(key), knownhostsline.Render(pattern, key), now, now, now); err != nil {
		t.Fatalf("approve host key for %s: %v", pattern, err)
	}
}

// trustHostKeyLine is trustHostKey for a key in authorized-key form; "" approves nothing.
func trustHostKeyLine(t *testing.T, pool *sql.DB, addr string, port int, authorizedKey string) {
	t.Helper()
	if strings.TrimSpace(authorizedKey) == "" {
		return
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		t.Fatalf("fixture host key: %v", err)
	}
	trustHostKey(t, pool, addr, port, key)
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

// ledgerRows counts the local runner's ledger rows: a connection test must
// never write one. It verifies; it does not capture.
func ledgerRows(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM host_key_ledger`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestProbeHost_UnknownKeyIsReportedNotCaptured(t *testing.T) {
	// No approved key for the host: the test reaches it, says its key is not
	// approved and where to approve it, and captures nothing. (Until 2.3.0 the
	// first connection stored whatever key the host presented.)
	svc, pool := probeFixture(t)
	client, clientPEM := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	storeKey(t, pool, "GOOD_KEY", clientPEM)
	insertHost(t, pool, "h1", host, atoiPort(port), "tester", "GOOD_KEY", "", "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil || res.Status != StatusUnverified {
		t.Fatalf("status = %q (%s) err=%v, want unverified", res.Status, res.Message, err)
	}
	if !strings.Contains(res.Message, "no approved host key") || !strings.Contains(res.Message, "Host keys") {
		t.Errorf("message = %q, want it to say the key is not approved and where to approve it", res.Message)
	}
	if n := ledgerRows(t, pool); n != 0 {
		t.Errorf("the test wrote %d ledger row(s): a test must not capture a key", n)
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

func TestProbeHost_Keyless_UnknownKeyIsReportedNotCaptured(t *testing.T) {
	// A target with no credential and no key name gets the unauthenticated tier.
	// With no approved key for it, the endpoint answers and the probe says its
	// key is not approved — never cred_error, and nothing is captured.
	svc, pool := probeFixture(t)
	client, _ := newKey(t)
	addr, _ := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)

	insertHost(t, pool, "h1", host, atoiPort(port), "", "", "", "")

	res, err := svc.ProbeHost(context.Background(), "h1")
	if err != nil {
		t.Fatalf("ProbeHost err: %v", err)
	}
	if res.Status != StatusUnverified {
		t.Fatalf("status = %q (%s), want unverified", res.Status, res.Message)
	}
	if n := ledgerRows(t, pool); n != 0 {
		t.Errorf("the reachability tier wrote %d ledger row(s): it must not capture a key", n)
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

func TestProbeBastion_Keyless(t *testing.T) {
	// A keyless bastion gets the same tier: unverified until its key is
	// approved for the local runner, reachable once it is.
	svc, pool := probeFixture(t)
	client, _ := newKey(t)
	addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
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
	if res.Status != StatusUnverified {
		t.Fatalf("status with no approved key = %q (%s), want unverified", res.Status, res.Message)
	}
	if n := ledgerRows(t, pool); n != 0 {
		t.Errorf("the test wrote %d ledger row(s): it must not capture the bastion's key", n)
	}

	trustHostKey(t, pool, host, atoiPort(port), hostKey)
	res, err = svc.ProbeBastion(context.Background(), "b1")
	if err != nil || res.Status != StatusReachable {
		t.Fatalf("status with the key approved = %q (%s) err=%v, want reachable", res.Status, res.Message, err)
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
	addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)
	trustHostKey(t, pool, host, atoiPort(port), hostKey)

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
	addr, hostKey := testSSHServer(t, serverClient.PublicKey(), "x")
	host, port, _ := net.SplitHostPort(addr)
	// The bastion's host key is approved: what is wrong here is the credential.
	trustHostKey(t, pool, host, atoiPort(port), hostKey)

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

// claimAsLocal makes svc the local runner the way Start does — its row exists,
// Global's and serving Global — and claims once. The engine has no claim query
// of its own since 2.3.0 (LR-40): it claims as that row, by the rule an agent's
// poll uses, so a test that wants a run claimed needs the row and a run the
// row may take.
func claimAsLocal(t *testing.T, svc *Service, pool *sql.DB) (*claimedRun, error) {
	t.Helper()
	ctx := context.Background()
	id, _, err := settings.EnsureLocalRunner(ctx, pool, true, 4)
	if err != nil {
		t.Fatalf("the local runner's row: %v", err)
	}
	svc.localID = id
	return svc.claim(ctx)
}
