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

// TestSSHExecutorRefusesSecretsOverUnpinnedBastion (SU-4 interim guard): a run that
// injects a secret and routes to an UNPINNED target over a bastion hop is failed
// closed BEFORE any dial — an unpinned target behind a bastion is only TOFU-trusted
// on first connect, a MITM window the injected secret must not cross.
func TestSSHExecutorRefusesSecretsOverUnpinnedBastion(t *testing.T) {
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
	// Target routed VIA a bastion and NOT pinned (host_key NULL).
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, via, created_at)
	      VALUES('h1','testhost','127.0.0.1',2222,'tester','SSH_KEY','jump',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO bastions(id, name, hostname, address, port, username, created_at)
	      VALUES('bj','jump','jumphost','127.0.0.1',2222,'jump',?)`, now); err != nil {
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

	var status, reason string
	if err := pool.QueryRow(`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE id='run-1'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failure" {
		t.Errorf("run status = %q, want failure (interim bastion guard)", status)
	}
	if reason != "unpinned_bastion_target" {
		t.Errorf("run reason = %q, want unpinned_bastion_target", reason)
	}
	logStr := readLog(t, logDir, "run-1")
	if !strings.Contains(logStr, "refusing to inject secrets over bastion") {
		t.Errorf("expected the refusal notice in the log:\n%s", logStr)
	}
	// No dial happened, so the secret value must never appear.
	if strings.Contains(logStr, secretVal) {
		t.Errorf("secret value leaked into the log:\n%s", logStr)
	}
}
