package sshexec

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// TestSSHExecutorDoesNotConnectThroughAnUnapprovedBastion. The SU-4 interim guard
// refused to inject secrets over a bastion to a target whose key had only been
// captured on first connect. Nothing is captured on first connect since 2.3.0:
// a hop the local runner has no approved key for is not connected to at all, so
// a run that injects a secret fails there — before the target is reached, and
// with the secret nowhere in the log.
func TestSSHExecutorDoesNotConnectThroughAnUnapprovedBastion(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	const scope = "testscope"
	const secretVal = "SECRET-DBPASS-XYZ"
	cfg := &config.Config{
		SSHExecutorEnabled:      true,
		SSHExecutorConcurrency:  2,
		SecretsInjectionEnabled: true,
		SecretKEKEnv:            "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	sec := secrets.New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := sec.Create(context.Background(), secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new(scope), Value: secretVal}, "tester"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	// Target routed VIA a bastion; no key is approved for either hop.
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, via, created_at)
	      VALUES('h1','testhost','127.0.0.1',2222,'tester','SSH_KEY','jump',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO bastions(id, name, hostname, address, port, username, created_at)
	      VALUES('bj','jump','jumphost','127.0.0.1',2222,'jump',?)`, now); err != nil {
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
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES('run-1','j1','cronomicon','bash',?,'testhost','queued','ops@x','manual','runner',?)`, scope, now); err != nil {
		t.Fatal(err)
	}

	logDir := t.TempDir()
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), logDir)
	endWG := &sync.WaitGroup{}
	svc.WithShutdownWG(endWG)
	t.Cleanup(endWG.Wait)
	claimed, err := claimAsLocal(t, svc, pool)
	if err != nil || claimed == nil {
		t.Fatalf("claim: run=%v err=%v", claimed, err)
	}
	svc.execute(context.Background(), *claimed)

	var status, reason string
	if err := pool.QueryRow(`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE id='run-1'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failure" && status != "warning" {
		t.Errorf("run status = %q, want a failure: the bastion's key is not approved", status)
	}
	logStr := readLog(t, logDir, "run-1")
	if strings.Contains(logStr, "cmd: ") {
		t.Errorf("the run reached the target and issued a command:\n%s", logStr)
	}
	// Nothing was sent to the target, so the secret value must never appear.
	if strings.Contains(logStr, secretVal) {
		t.Errorf("secret value leaked into the log:\n%s", logStr)
	}
}
