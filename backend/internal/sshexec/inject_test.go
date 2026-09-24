package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"golang.org/x/crypto/ssh"
)

// echoCommandSSHServer is a minimal sshd that, on exec, streams back the exact
// command string it received AND the bytes it received on stdin — so a test can
// verify the H1 property that injected env rides on stdin, never in the command
// line (argv).
func echoCommandSSHServer(t *testing.T, clientPub ssh.PublicKey) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
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
			go serveEcho(conn, cfg)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func serveEcho(nConn net.Conn, cfg *ssh.ServerConfig) {
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
					var execReq struct{ Command string }
					_ = ssh.Unmarshal(req.Payload, &execReq)
					// H1: env (incl. secrets) is delivered on stdin; drain it to EOF
					// (the client closes its write side once the prelude is sent) and
					// echo both streams back so the test can inspect each separately.
					stdin, _ := io.ReadAll(ch)
					io.WriteString(ch, "cmd: "+execReq.Command+"\n")
					if len(stdin) > 0 {
						io.WriteString(ch, "stdin:\n"+string(stdin)+"\n")
					}
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					ch.Close()
					return
				}
				req.Reply(false, nil)
			}
		}()
	}
}

// TestSSHExecutorInjectsReferences is the P1.3 end-to-end: a job declares a secret
// + a variable reference binding; the executor resolves them at dispatch and
// injects the derived AMADEUS_SECRET_*/AMADEUS_VAR_* values plus the AMADEUS_RUN_*
// context onto the remote command — and the injected secret VALUE is masked in the
// log while the log-safe variable value is not.
func TestSSHExecutorInjectsReferences(t *testing.T) {
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
	pemBlock, _ := ssh.MarshalPrivateKey(clientPriv, "")
	pemBytes := pem.EncodeToMemory(pemBlock)

	addr, hostKey := echoCommandSSHServer(t, clientSigner.PublicKey())
	host, port, _ := net.SplitHostPort(addr)

	now := time.Now().UTC().Format(time.RFC3339)
	const scope = "testscope"
	const secretVal = "SECRET-DBPASS-VALUE-XYZ"
	const regionVal = "us-east-2"

	cfg := &config.Config{
		SSHExecutorEnabled:      true,
		SSHExecutorConcurrency:  2,
		SecretsInjectionEnabled: true,
		SecretKEKEnv:            "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}

	// Connection key (env_vars holds the PEM the host references by name).
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY',?,?,?)`,
		scope, string(pemBytes), now); err != nil {
		t.Fatal(err)
	}
	// A bound VARIABLE (log-safe value) + a bound stored SECRET (masked value).
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e2','REGION',?,?,?)`,
		scope, regionVal, now); err != nil {
		t.Fatal(err)
	}
	// A variable the job does NOT declare — attached per-run via the override
	// envelope (V2-11 stored-reference additions).
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e3','EXTRA',?,?,?)`,
		scope, "extra-value", now); err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc, err := sec.Create(context.Background(), secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new(scope), Value: secretVal}, "tester")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	_ = sc

	// Host + amadeus job + the job's reference bindings.
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1', 'testhost', ?, ?, 'tester', 'SSH_KEY', ?, ?)`,
		host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	// Bind a secret and a variable. (A bound KEY is a different story on this
	// executor — it fails the run before connecting; see
	// TestSSHExecutorFailsBeforeConnectingOnKeyBinding.)
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j1','secret','DB_PASS',?),('job','amadeus','j1','var','REGION',?)`, now, now); err != nil {
		t.Fatal(err)
	}
	// The run carries a per-run reference ADDITION in its override envelope: the
	// undeclared EXTRA variable.
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, override_json, created_at)
		VALUES('run-1','j1','amadeus','bash',?,'testhost','queued','ops@x','manual','ssh',
		       '{"references":[{"kind":"var","name":"EXTRA"}]}',?)`, scope, now); err != nil {
		t.Fatal(err)
	}

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

	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "success" {
		t.Fatalf("run status = %q, want success", status)
	}

	logStr := readLog(t, logDir, "run-1")

	// H1: the injected env is delivered on stdin, so the command line is the bare
	// stdin-reader form — no "K='v'" prefix carrying values into argv.
	if !strings.Contains(logStr, "cmd: bash -s") {
		t.Errorf("expected the stdin-reader command form (bash -s):\n%s", logStr)
	}

	// Injection reached the run via the stdin export prelude — the derived
	// reference keys and the fixed run-context set are all present. (Values are
	// asserted separately: AMADEUS_RUN_* renders verbatim; the reference VALUES are
	// masked by the log redactor — see below.)
	for _, want := range []string{
		"export AMADEUS_SECRET_DB_PASS=",
		"export AMADEUS_VAR_REGION=",
		// V2-11 — the per-run ADDED variable rides the same prelude as declared ones.
		"export AMADEUS_VAR_EXTRA=",
		"export AMADEUS_RUN_ID='run-1'",
		"export AMADEUS_RUN_JOB='j1'",
		"export AMADEUS_RUN_SCOPE='" + scope + "'",
		"export AMADEUS_RUN_TRIGGERED_BY='ops@x'",
		"export AMADEUS_RUN_EXECUTOR='ssh'",
	} {
		if !strings.Contains(logStr, want) {
			t.Errorf("expected %q in the stdin prelude:\n%s", want, logStr)
		}
	}

	// H1 core property: the injected secret VALUE must not appear on the command
	// line (argv) even before redaction. The echoed "cmd: " line carries the argv;
	// assert the value is confined to the stdin section.
	for line := range strings.SplitSeq(logStr, "\n") {
		if strings.HasPrefix(line, "cmd: ") && strings.Contains(line, "AMADEUS_SECRET") {
			t.Errorf("injected env leaked onto the command line (argv): %q", line)
		}
	}
	// The injected secret VALUE never surfaces un-masked. (The variable value is
	// ALSO masked here — not because the resolver flags it, but because the SSH
	// executor's baseline redactor independently loads every scope env_vars value.
	// The resolver's own Redact slice correctly excludes log-safe variable values,
	// per D7; that contract is proven in runref's TestResolveHappyPath. At this
	// executor the extra masking is redundant but strictly safe.)
	if strings.Contains(logStr, secretVal) {
		t.Errorf("injected secret value leaked into log:\n%s", logStr)
	}
	if !strings.Contains(logStr, "[REDACTED]") {
		t.Errorf("expected redaction marker in log:\n%s", logStr)
	}
	// Dispatch audit (P1.6): exactly one change_log injection row for this run,
	// recording the reference names + source, never the value.
	var count int
	var details string
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target='run-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 injection audit row, got %d", count)
	}
	_ = pool.QueryRow(`SELECT details FROM change_log WHERE category='Secrets' AND action='injected' AND target='run-1'`).Scan(&details)
	if !strings.Contains(details, "DB_PASS") || !strings.Contains(details, "REGION") {
		t.Errorf("audit details missing reference names: %s", details)
	}
	if !strings.Contains(details, "EXTRA") {
		t.Errorf("audit details missing the per-run added reference: %s", details)
	}
	if strings.Contains(details, secretVal) {
		t.Errorf("audit details leaked the secret value: %s", details)
	}
	var audited bool
	_ = pool.QueryRow(`SELECT injection_audited FROM runs WHERE id='run-1'`).Scan(&audited)
	if !audited {
		t.Errorf("injection_audited not set after a successful audited run")
	}
}

// TestSSHExecutorFailsClosedOnMissingBinding proves dispatch halts (fail-closed)
// when a declared binding cannot be resolved, rather than running without it.
func TestSSHExecutorFailsClosedOnMissingBinding(t *testing.T) {
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
	pemBlock, _ := ssh.MarshalPrivateKey(clientPriv, "")
	pemBytes := pem.EncodeToMemory(pemBlock)
	addr, hostKey := echoCommandSSHServer(t, clientSigner.PublicKey())
	host, port, _ := net.SplitHostPort(addr)

	now := time.Now().UTC().Format(time.RFC3339)
	cfg := &config.Config{SSHExecutorEnabled: true, SecretsInjectionEnabled: true,
		SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY','s',?,?)`,
		string(pemBytes), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1','testhost',?,?,'tester','SSH_KEY',?,?)`, host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	// Binding names a secret that does not exist → resolution must fail closed.
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j1','secret','NOPE',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('run-1','j1','amadeus','bash','s','testhost','queued','ops@x','manual','ssh',?)`, now); err != nil {
		t.Fatal(err)
	}

	logDir := t.TempDir()
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), logDir)
	endWG := &sync.WaitGroup{}
	svc.WithShutdownWG(endWG)
	t.Cleanup(endWG.Wait)

	claimed, err := svc.claim(context.Background())
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	svc.execute(context.Background(), *claimed)

	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failure" {
		t.Fatalf("run status = %q, want failure (fail-closed on missing binding)", status)
	}
	logStr := readLog(t, logDir, "run-1")
	if !strings.Contains(logStr, "reference injection failed") {
		t.Errorf("expected fail-closed message in log:\n%s", logStr)
	}
	// The command must NOT have run: the echo server prints "cmd: ..." only on exec.
	if strings.Contains(logStr, "cmd: ") {
		t.Errorf("remote command ran despite failed injection:\n%s", logStr)
	}
}

// TestSSHExecutorFailsClosedOnAuditError (P1.6): with a valid, resolvable secret
// binding but a broken audit sink, execute() must halt the run (fail-closed) BEFORE
// running the remote command — a reference is never injected without an audit trail.
func TestSSHExecutorFailsClosedOnAuditError(t *testing.T) {
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
	pemBlock, _ := ssh.MarshalPrivateKey(clientPriv, "")
	pemBytes := pem.EncodeToMemory(pemBlock)
	addr, hostKey := echoCommandSSHServer(t, clientSigner.PublicKey())
	host, port, _ := net.SplitHostPort(addr)

	now := time.Now().UTC().Format(time.RFC3339)
	const scope = "s"
	cfg := &config.Config{SSHExecutorEnabled: true, SecretsInjectionEnabled: true,
		SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY',?,?,?)`,
		scope, string(pemBytes), now); err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := sec.Create(context.Background(), secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new(scope), Value: "resolvable"}, "tester"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1','testhost',?,?,'tester','SSH_KEY',?,?)`, host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j1','secret','DB_PASS',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('run-1','j1','amadeus','bash',?,'testhost','queued','ops@x','manual','ssh',?)`, scope, now); err != nil {
		t.Fatal(err)
	}
	// Break the audit sink so the injection audit write fails.
	if _, err := pool.Exec(`DROP TABLE change_log`); err != nil {
		t.Fatalf("drop change_log: %v", err)
	}

	logDir := t.TempDir()
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), logDir)
	endWG := &sync.WaitGroup{}
	svc.WithShutdownWG(endWG)
	t.Cleanup(endWG.Wait)

	claimed, err := svc.claim(context.Background())
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	svc.execute(context.Background(), *claimed)

	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failure" {
		t.Fatalf("run status = %q, want failure (fail-closed on audit error)", status)
	}
	logStr := readLog(t, logDir, "run-1")
	if !strings.Contains(logStr, "dispatch audit failed") {
		t.Errorf("expected in-band audit-failure notice in log:\n%s", logStr)
	}
	// The remote command must NOT have run (echo server prints "cmd: " only on exec).
	if strings.Contains(logStr, "cmd: ") {
		t.Errorf("remote command ran despite failed audit:\n%s", logStr)
	}
}

//go:fix inline

func readLog(t *testing.T, dir, trace string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, trace+".log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSSHExecutorFailsBeforeConnectingOnKeyBinding is the KB executor-side half:
// a bound SSH key that reaches dispatch on this executor (a binding added while
// the run waited — every producer refuses one at enqueue) fails the run BEFORE
// it connects, with the reason in the run log and the injection audit row still
// written. It used to warn in-band and run anyway.
func TestSSHExecutorFailsBeforeConnectingOnKeyBinding(t *testing.T) {
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
	pemBlock, _ := ssh.MarshalPrivateKey(clientPriv, "")
	pemBytes := pem.EncodeToMemory(pemBlock)

	addr, hostKey := echoCommandSSHServer(t, clientSigner.PublicKey())
	host, port, _ := net.SplitHostPort(addr)

	now := time.Now().UTC().Format(time.RFC3339)
	const scope = "testscope"
	cfg := &config.Config{
		SSHExecutorEnabled:      true,
		SSHExecutorConcurrency:  2,
		SecretsInjectionEnabled: true,
		SecretKEKEnv:            "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY',?,?,?)`,
		scope, string(pemBytes), now); err != nil {
		t.Fatal(err)
	}
	// A REAL stored credential, so the resolver returns key material rather than
	// failing on a missing binding — the KB guard is what must fail the run.
	keys := sshkeys.New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := keys.Create(context.Background(), sshkeys.CreateInput{Label: "deploy_key", Source: "stored", Material: string(pemBytes)}, "tester"); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1', 'testhost', ?, ?, 'tester', 'SSH_KEY', ?, ?)`,
		host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "job", Source: "amadeus", Name: "j1"},
		[]runref.Binding{{Kind: runref.KindKey, Name: "deploy_key"}}, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('run-1','j1','amadeus','bash',?,'testhost','queued','ops@x','manual','ssh',?)`, scope, now); err != nil {
		t.Fatal(err)
	}

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

	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failure" {
		t.Fatalf("run status = %q, want failure — the executor cannot deliver the key, so the run must not start", status)
	}
	logStr := readLog(t, logDir, "run-1")
	if !strings.Contains(logStr, "AMADEUS_KEY_deploy_key") || !strings.Contains(logStr, "cannot deliver key files") {
		t.Errorf("run log should name the key and say why:\n%s", logStr)
	}
	if strings.Contains(logStr, "cmd: ") {
		t.Errorf("the run connected and issued a command — it must fail BEFORE dialing:\n%s", logStr)
	}
	if strings.Contains(logStr, string(pemBytes)) {
		t.Errorf("key material leaked into the run log")
	}
	// The injection audit still records what was declared (same order as the
	// missing-binding path: audit, then fail).
	var count int
	var details string
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target='run-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 injection audit row, got %d", count)
	}
	_ = pool.QueryRow(`SELECT details FROM change_log WHERE category='Secrets' AND action='injected' AND target='run-1'`).Scan(&details)
	if !strings.Contains(details, "deploy_key") {
		t.Errorf("audit details missing the key reference: %s", details)
	}
}
