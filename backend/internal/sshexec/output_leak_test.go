package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"golang.org/x/crypto/ssh"
)

// outputMarkerSSHServer is a minimal sshd that, on exec, drains stdin (the env
// prelude the executor delivers on stdin) and then emits a single
// ::cronomicon-output:: marker line carrying markerValue on stdout before exiting 0.
// It lets a test drive the exact SU-1 leak idiom
// (`echo "::cronomicon-output name=TOKEN::$CRONOMICON_SECRET_DB_PASS"`) without a real
// remote shell: pass the injected secret value as markerValue.
func outputMarkerSSHServer(t *testing.T, clientPub ssh.PublicKey, markerValue string) (addr string, hostKey ssh.PublicKey) {
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
			go serveOutputMarker(conn, cfg, markerValue)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func serveOutputMarker(nConn net.Conn, cfg *ssh.ServerConfig, markerValue string) {
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
					// Drain the stdin env prelude to EOF (client closes its write side),
					// then emit the leaking output marker on stdout.
					_, _ = io.ReadAll(ch)
					_, _ = io.WriteString(ch, "::cronomicon-output name=TOKEN::"+markerValue+"\n")
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					ch.Close()
					return
				}
				req.Reply(false, nil)
			}
		}()
	}
}

// TestSSHExecutorRefusesOutputLeakingSecret is the SU-1 fix: the in-app SSH
// executor, like the runner log-ingest path, must fail a run CLOSED when a
// captured ::cronomicon-output:: value carries an injected secret — the value is
// never persisted to outputs_json (so nothing can propagate into a child step's
// env_json or the run-detail API), the run is failed with reason
// output_secret_leak, and the secret stays masked in the log.
func TestSSHExecutorRefusesOutputLeakingSecret(t *testing.T) {
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

	now := time.Now().UTC().Format(time.RFC3339)
	const scope = "testscope"
	const secretVal = "SECRET-DBPASS-VALUE-XYZ"

	// The remote "job" echoes the injected secret value into an output marker.
	addr, hostKey := outputMarkerSSHServer(t, clientSigner.PublicKey(), secretVal)
	host, port, _ := net.SplitHostPort(addr)

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
	// A bound stored SECRET whose VALUE the remote will echo into an output.
	sec := secrets.New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := sec.Create(context.Background(), secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new(scope), Value: secretVal}, "tester"); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, host_key, created_at)
		VALUES('h1', 'testhost', ?, ?, 'tester', 'SSH_KEY', ?, ?)`,
		host, atoiPort(port), string(ssh.MarshalAuthorizedKey(hostKey)), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','cronomicon','bash','echo hi','Allow',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','cronomicon','j1','secret','DB_PASS',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('run-1','j1','cronomicon','bash',?,'testhost','queued','ops@x','manual','ssh',?)`, scope, now); err != nil {
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

	// The run is failed CLOSED with the distinct reason.
	var status, reason string
	if err := pool.QueryRow(`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE id='run-1'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failure" {
		t.Errorf("run status = %q, want failure (fail-closed on output leak)", status)
	}
	if reason != "output_secret_leak" {
		t.Errorf("run reason = %q, want output_secret_leak", reason)
	}

	// The output value must NOT have been persisted — nothing can carry it into a
	// child step's env_json or the run-detail API.
	var outputs string
	_ = pool.QueryRow(`SELECT COALESCE(outputs_json,'') FROM runs WHERE id='run-1'`).Scan(&outputs)
	if outputs != "" {
		t.Errorf("leaking output persisted to outputs_json: %q", outputs)
	}

	// The secret is still masked in the log, and a refusal notice names the output.
	logStr := readLog(t, logDir, "run-1")
	if strings.Contains(logStr, secretVal) {
		t.Errorf("secret value leaked into persisted log:\n%s", logStr)
	}
	if !strings.Contains(logStr, `output "TOKEN" would leak an injected secret`) {
		t.Errorf("expected refusal notice naming the output:\n%s", logStr)
	}
}
