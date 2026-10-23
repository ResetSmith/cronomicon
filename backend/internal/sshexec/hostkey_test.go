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

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// The local runner connects only to a host whose key an operator has approved
// for it (2.3.0, Phase C). A host with no approved key is not connected to: the
// run fails for it, host_key_unverified, with where to approve the key — and
// nothing is captured, which is what the first connection did until 2.3.0. Once
// the key is approved, the same run goes through; and a host that then presents
// a different key is refused as a mismatch.
func TestARunConnectsOnlyToAHostWithAnApprovedKey(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "hk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	addr, hostKey := testSSHServer(t, clientSigner.PublicKey(), "ok")
	host, port, _ := net.SplitHostPort(addr)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e1','SSH_KEY','testscope',?,?)`,
			[]any{string(pem.EncodeToMemory(pemBlock)), now}},
		{`INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, created_at)
		  VALUES('h1', 'testhost', ?, ?, 'tester', 'SSH_KEY', ?)`, []any{host, atoiPort(port), now}},
		{`INSERT INTO jobs(name, run_type, command, concurrency_policy, synced_at) VALUES('j1','bash','echo hi','Allow',?)`, []any{now}},
	} {
		if _, err := pool.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q.sql)
		}
	}
	cfg := &config.Config{SSHExecutorEnabled: true, SSHExecutorConcurrency: 2,
		SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	logDir := t.TempDir()
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), logDir)
	endWG := &sync.WaitGroup{}
	svc.WithShutdownWG(endWG)
	t.Cleanup(endWG.Wait)

	run := func(id string) (status, log string) {
		t.Helper()
		if _, err := pool.Exec(`
			INSERT INTO runs(id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
			VALUES(?,'j1','bash','testscope','testhost','queued','tester','manual','runner',?)`, id, now); err != nil {
			t.Fatal(err)
		}
		claimed, err := claimAsLocal(t, svc, pool)
		if err != nil || claimed == nil {
			t.Fatalf("claim %s: run=%v err=%v", id, claimed, err)
		}
		svc.execute(context.Background(), *claimed)
		if err := pool.QueryRow(`SELECT status FROM runs WHERE id = ?`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status, readLog(t, logDir, id)
	}
	ledger := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM host_key_ledger`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// No approved key.
	status, log := run("run-unverified")
	if status != "failure" {
		t.Fatalf("with no approved key the run is %q, want failure\n%s", status, log)
	}
	if !strings.Contains(log, HostKeyUnverified) || !strings.Contains(log, "Host keys") {
		t.Errorf("the log must name the cause and where the key is approved:\n%s", log)
	}
	if strings.Contains(log, "cmd: ") {
		t.Errorf("the run issued a command on a host whose key is not approved:\n%s", log)
	}
	if n := ledger(); n != 0 {
		t.Fatalf("the connection wrote %d ledger row(s): nothing is captured on first connect", n)
	}
	// History shows why, not only the log.
	var reason string
	_ = pool.QueryRow(`SELECT COALESCE(queued_reason, '') FROM runs WHERE id = 'run-unverified'`).Scan(&reason)
	if reason != HostKeyUnverified+": testhost" {
		t.Errorf("the run's reason = %q, want %q", reason, HostKeyUnverified+": testhost")
	}

	// An operator approves the key the host presents.
	trustHostKey(t, pool, host, atoiPort(port), hostKey)
	if status, log := run("run-approved"); status != "success" {
		t.Fatalf("with the key approved the run is %q, want success\n%s", status, log)
	}

	// The approved key is replaced by another: the host's real key no longer
	// matches what is in force, and the run says so rather than connecting.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	if _, err := pool.Exec(`UPDATE host_key_ledger SET superseded_at = ?, untrusted_at = ?`, now, now); err != nil {
		t.Fatal(err)
	}
	trustHostKey(t, pool, host, atoiPort(port), other.PublicKey())
	status, log = run("run-mismatch")
	if status != "failure" || !strings.Contains(log, "mismatch") {
		t.Errorf("with a different key in force the run is %q, want a failure naming the mismatch\n%s", status, log)
	}
}
