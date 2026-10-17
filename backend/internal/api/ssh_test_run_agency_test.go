package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
)

// Present defect 71 (C0): an SSH host's test is recorded as a run with no job,
// which 1480's trigger made Global's whatever the host's agency, so FIN's host
// test was in Global's History and Activity and not in FIN's. The run and its
// Activity row are the host's agency's.
func TestC1_AnSSHHostTestIsTheHostsAgencys(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "sshtest.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag:FIN', 'Finance', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO ssh_hosts (id, hostname, owner_agency, created_at) VALUES ('h-fin', 'fin-host', 'ag:FIN', ?)`, now); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dir := t.TempDir()
	s.logDir.Store(&dir)

	s.recordSSHTestRun(ctx, settings.RowAgency(ctx, pool, "ssh_hosts", "owner_agency", "h-fin"), "fin-host",
		sshexec.ProbeResult{Status: sshexec.StatusVerified, CheckedAt: now}, "alice@example")

	var runOwner, actAgency string
	if err := pool.QueryRow(`SELECT owner_agency FROM runs WHERE kind = 'ssh-test'`).Scan(&runOwner); err != nil {
		t.Fatalf("the test's run: %v", err)
	}
	if err := pool.QueryRow(`SELECT agency_id FROM activity WHERE actor = 'ssh-test'`).Scan(&actAgency); err != nil {
		t.Fatalf("the test's Activity row: %v", err)
	}
	if runOwner != "ag:FIN" || actAgency != "ag:FIN" {
		t.Errorf("FIN's host test: run %s's, Activity %s's; want ag:FIN's", runOwner, actAgency)
	}
}
