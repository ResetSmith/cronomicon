package sshexec

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// auditTestService builds a minimal SSH executor Service over a fresh migrated DB
// plus a seeded run row, for exercising auditInjection in isolation.
func auditTestService(t *testing.T, traceID, triggeredBy, scope string) *Service {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, scope, triggered_by, trigger_kind, executor, created_at)
		VALUES(?, 'j1', 'bash', 'running', ?, ?, 'manual', 'ssh', ?)`, traceID, scope, triggeredBy, now); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SSHExecutorEnabled: true, SecretsInjectionEnabled: true}
	return New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), t.TempDir())
}

// TestSSHAuditInjectionIdempotent (P1.6): auditInjection writes one row and, on a
// second call for the same run (belt-and-suspenders vs re-entry), does not duplicate.
func TestSSHAuditInjectionIdempotent(t *testing.T) {
	svc := auditTestService(t, "run-a", "ops@x", "prod")
	r := claimedRun{traceID: "run-a", triggeredBy: "ops@x", scope: "prod"}
	resolved := &runref.Resolved{Refs: []runref.ResolvedRef{
		{Kind: runref.KindSecret, Name: "DB_PASS", Source: "stored"},
	}}

	if err := svc.auditInjection(context.Background(), r, resolved); err != nil {
		t.Fatalf("auditInjection: %v", err)
	}
	if err := svc.auditInjection(context.Background(), r, resolved); err != nil {
		t.Fatalf("auditInjection (2nd): %v", err)
	}
	var count int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action='injected' AND target='run-a'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 audit row after two calls, got %d", count)
	}
	var actor string
	_ = svc.db.QueryRow(`SELECT actor FROM change_log WHERE action='injected' AND target='run-a'`).Scan(&actor)
	if actor != "ops@x" {
		t.Errorf("actor = %q, want ops@x", actor)
	}
}

// TestSSHAuditInjectionUnattributed: a run with no triggered_by is audited as
// "system" rather than an empty actor.
func TestSSHAuditInjectionUnattributed(t *testing.T) {
	svc := auditTestService(t, "run-b", "", "prod")
	r := claimedRun{traceID: "run-b", triggeredBy: "", scope: "prod"}
	resolved := &runref.Resolved{Refs: []runref.ResolvedRef{{Kind: runref.KindVar, Name: "REGION", Source: "stored"}}}
	if err := svc.auditInjection(context.Background(), r, resolved); err != nil {
		t.Fatalf("auditInjection: %v", err)
	}
	var actor string
	_ = svc.db.QueryRow(`SELECT actor FROM change_log WHERE action='injected' AND target='run-b'`).Scan(&actor)
	if actor != "system" {
		t.Errorf("unattributed actor = %q, want system", actor)
	}
}

// TestSSHAuditInjectionFailsClosed (P1.6): a broken audit sink makes auditInjection
// return an error (execute() then fails the run) and leaves injection_audited unset.
func TestSSHAuditInjectionFailsClosed(t *testing.T) {
	svc := auditTestService(t, "run-c", "ops@x", "prod")
	if _, err := svc.db.Exec(`DROP TABLE change_log`); err != nil {
		t.Fatalf("drop change_log: %v", err)
	}
	r := claimedRun{traceID: "run-c", triggeredBy: "ops@x", scope: "prod"}
	resolved := &runref.Resolved{Refs: []runref.ResolvedRef{{Kind: runref.KindSecret, Name: "DB_PASS", Source: "stored"}}}
	if err := svc.auditInjection(context.Background(), r, resolved); err == nil {
		t.Fatal("expected auditInjection to fail closed when the audit sink is broken")
	}
	var audited bool
	_ = svc.db.QueryRow(`SELECT injection_audited FROM runs WHERE id='run-c'`).Scan(&audited)
	if audited {
		t.Errorf("injection_audited must not be set when the audit write failed")
	}
}
