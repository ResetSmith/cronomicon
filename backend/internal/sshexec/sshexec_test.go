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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"golang.org/x/crypto/ssh"
)

// testSSHServer is a minimal in-process SSH server: it accepts the given client
// public key, and on an "exec" request streams two lines (one carrying a secret
// to prove redaction) then exits 0. Returns its listen address + host key.
func testSSHServer(t *testing.T, clientPub ssh.PublicKey, secret string) (addr string, hostKey ssh.PublicKey) {
	return testSSHServerOpts(t, clientPub, secret, false)
}

// testSSHServerOpts is testSSHServer with a `block` mode: when block is true the
// exec handler writes one line then blocks (never sending exit-status) until the
// client closes the session — simulating a hung/non-emitting remote command so
// PP-H1's ctx-cancellation guard (timeout / kill) can be exercised.
func testSSHServerOpts(t *testing.T, clientPub ssh.PublicKey, secret string, block bool) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg, secret, block)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func serveConn(nConn net.Conn, cfg *ssh.ServerConfig, secret string, block bool) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					req.Reply(true, nil)
					io.WriteString(ch, "starting job\n")
					if block {
						// Hung command: emit one line then send NOTHING — no
						// exit-status, don't close the channel. Keep the request
						// loop alive (it blocks on the next `range chReqs`) so the
						// channel stays open and the client's session.Run blocks
						// until the PP-H1 guard closes the session (timeout/kill),
						// which closes chReqs and ends this loop.
						continue
					}
					io.WriteString(ch, "token is "+secret+"\n")
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					ch.Close()
					return
				}
				req.Reply(false, nil)
			}
		}()
	}
}

func TestSSHExecutorEndToEnd(t *testing.T) {
	// DB with full schema.
	pool, err := db.Open(filepath.Join(t.TempDir(), "ssh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}

	// Client key the executor will authenticate with.
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)

	const secret = "SUPERSECRET-TOKEN"
	addr, hostKey := testSSHServer(t, clientSigner.PublicKey(), secret)
	host, port, _ := net.SplitHostPort(addr)

	now := time.Now().UTC().Format(time.RFC3339)
	// The private key lives in env_vars under the name the host references.
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY','testscope',?,?)`,
		string(ssh.MarshalAuthorizedKey(clientSigner.PublicKey())), now); err != nil {
		t.Fatal(err)
	}
	// Overwrite with the actual private PEM (env_vars.value holds it).
	if _, err := pool.Exec(`UPDATE env_vars SET value=? WHERE id='e1'`, string(pemBytes)); err != nil {
		t.Fatal(err)
	}
	// A plain Variable whose value the remote echoes — proves single-line
	// Variables are log-visible (D7), not redacted.
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e2','APP_TOKEN','testscope',?,?)`,
		secret, now); err != nil {
		t.Fatal(err)
	}
	// SSH host record (strict host-key verification using the server's key).
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1', ?, ?, ?, 'tester', 'SSH_KEY', ?, ?)`,
		"testhost", host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	// Job with an inline command + a queued ssh run targeting the host.
	if _, err := pool.Exec(`INSERT INTO jobs(name, run_type, command, concurrency_policy, synced_at) VALUES('j1','bash','echo hi','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('run-1','j1','bash','testscope','testhost','queued','tester','manual','ssh',?)`, now); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{SSHExecutorEnabled: true, SSHExecutorConcurrency: 2,
		SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	logDir := t.TempDir()
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), logDir)
	endWG := &sync.WaitGroup{}
	svc.WithShutdownWG(endWG)
	t.Cleanup(endWG.Wait)

	claimed, err := svc.claim(context.Background())
	if err != nil || claimed == nil {
		t.Fatalf("claim: run=%v err=%v", claimed, err)
	}
	svc.execute(context.Background(), *claimed)

	// Run finalized success.
	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "success" {
		t.Fatalf("run status = %q, want success", status)
	}

	// duration_ms is recorded at finalize (LB1): non-NULL and non-negative.
	var durMs sql.NullInt64
	if err := pool.QueryRow(`SELECT duration_ms FROM runs WHERE id='run-1'`).Scan(&durMs); err != nil {
		t.Fatal(err)
	}
	if !durMs.Valid || durMs.Int64 < 0 {
		t.Fatalf("duration_ms = %v (valid=%v), want a recorded non-negative elapsed time", durMs.Int64, durMs.Valid)
	}

	// Log captured output, host-prefixed, with the secret redacted (shared seam).
	logBytes, err := os.ReadFile(filepath.Join(logDir, "run-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "[testhost] starting job") {
		t.Errorf("log missing host-prefixed output:\n%s", logStr)
	}
	// Single-line Variables (env_vars) are log-visible by design (D7) — the
	// echoed APP_TOKEN value must appear raw. Injected-secret masking through
	// this same seam is covered by inject_test.go.
	if !strings.Contains(logStr, secret) {
		t.Errorf("Variable value should be log-visible (D7), but is missing/masked:\n%s", logStr)
	}
	if strings.Contains(logStr, "[REDACTED]") {
		t.Errorf("unexpected redaction marker for a plain Variable:\n%s", logStr)
	}
}

func TestSupportedRunType(t *testing.T) {
	for _, rt := range []string{"bash", "perl", "powershell", "python"} {
		if !SupportedRunType(rt) {
			t.Errorf("%s should be SSH-supported", rt)
		}
	}
	for _, rt := range []string{"ansible", "terraform"} {
		if SupportedRunType(rt) {
			t.Errorf("%s should NOT be SSH-supported in v1", rt)
		}
	}
}

// setupBlockingRun wires a DB + a BLOCKING in-process SSH server (the remote
// command never exits on its own — only ctx cancellation can end it) + a queued
// ssh run, and returns the executor service + pool. timeoutSeconds sets the
// job's A12 timeout (0 = none). Mirrors TestSSHExecutorEndToEnd's seed.
func setupBlockingRun(t *testing.T, timeoutSeconds int) (*Service, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "ssh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}

	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)

	addr, hostKey := testSSHServerOpts(t, clientSigner.PublicKey(), "", true)
	host, port, _ := net.SplitHostPort(addr)
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY','testscope',?,?)`,
		string(pemBytes), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1', ?, ?, ?, 'tester', 'SSH_KEY', ?, ?)`,
		"testhost", host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, run_type, command, concurrency_policy, timeout_seconds, synced_at) VALUES('j1','bash','sleep 999','Allow',?,?)`,
		timeoutSeconds, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('run-1','j1','bash','testscope','testhost','queued','tester','manual','ssh',?)`, now); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{SSHExecutorEnabled: true, SSHExecutorConcurrency: 2,
		SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), t.TempDir())
	// Drain the async terminal-notify dispatch before the pool closes (LIFO
	// t.Cleanup → runs before the pool.Close registered above).
	wg := &sync.WaitGroup{}
	svc.WithShutdownWG(wg)
	t.Cleanup(wg.Wait)
	return svc, pool
}

// runExecuteWithDeadline runs execute in a goroutine and fails if it does not
// return within d — proving the blocking session.Run was interrupted (and the
// worker goroutine + concurrency slot were released, not leaked).
func runExecuteWithDeadline(t *testing.T, svc *Service, r claimedRun, d time.Duration) {
	t.Helper()
	doneCh := make(chan struct{})
	go func() { svc.execute(context.Background(), r); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(d):
		t.Fatalf("execute did not return within %v — session.Run was not interrupted (goroutine + slot leaked)", d)
	}
}

// TestSSHExecutorTimeoutInterruptsRun (PP-H1): a job with a 1s A12 timeout whose
// remote command hangs is interrupted, finalizes failure, and logs the timeout.
func TestSSHExecutorTimeoutInterruptsRun(t *testing.T) {
	svc, pool := setupBlockingRun(t, 1)
	claimed, err := svc.claim(context.Background())
	if err != nil || claimed == nil {
		t.Fatalf("claim: run=%v err=%v", claimed, err)
	}
	runExecuteWithDeadline(t, svc, *claimed, 10*time.Second)

	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failure" {
		t.Errorf("status = %q, want failure after timeout", status)
	}
	logBytes, _ := os.ReadFile(filepath.Join(svc.LogDir(), "run-1.log"))
	if !strings.Contains(string(logBytes), "cronomicon: job timed out") {
		t.Errorf("log missing timeout marker:\n%s", logBytes)
	}
}

// TestSSHExecutorKillInterruptsRun (PP-H1): an operator kill queued mid-run
// actually interrupts the hung command (not just a false "killed" log), the run
// finalizes terminal, and the kill signal is consumed.
func TestSSHExecutorKillInterruptsRun(t *testing.T) {
	svc, pool := setupBlockingRun(t, 0)
	claimed, err := svc.claim(context.Background())
	if err != nil || claimed == nil {
		t.Fatalf("claim: run=%v err=%v", claimed, err)
	}
	// watchKill polls action_queue every 2s; queue the kill before executing.
	if _, err := pool.Exec(`INSERT INTO action_queue(run_id, op, created_at) VALUES('run-1','kill',?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	runExecuteWithDeadline(t, svc, *claimed, 15*time.Second)

	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failure" && status != "warning" {
		t.Errorf("status = %q, want failure/warning after kill", status)
	}
	var consumed sql.NullString
	if err := pool.QueryRow(`SELECT consumed_at FROM action_queue WHERE run_id='run-1'`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if !consumed.Valid {
		t.Errorf("kill signal not marked consumed")
	}
}

func atoiPort(p string) int {
	n := 0
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return n
}
