package seed

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

func TestSeedPopulatesEveryView(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	if err := Seed(ctx, pool, log); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Every table a view reads from must be non-empty.
	for _, tbl := range []string{
		"scopes", "scope_hosts", "access_grants",
		"recent_logins", "jobs", "workflows", "runs", "workflow_runs",
		"activity", "change_log", "schedule_pushes", "git_sync_events",
		"runners", "env_vars", "secrets", "ssh_hosts", "bastions", "ssh_credentials",
		"alert_destinations", "alert_config", "settings",
	} {
		var n int
		if err := pool.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n == 0 {
			t.Errorf("table %s is empty after seed", tbl)
		}
	}

	// Singletons present.
	for _, tbl := range []string{"notification_config", "gitlab_config", "git_sync_state"} {
		var n int
		_ = pool.QueryRow("SELECT COUNT(*) FROM " + tbl + " WHERE id=1").Scan(&n)
		if n != 1 {
			t.Errorf("singleton %s missing", tbl)
		}
	}

	// Re-seeding is a no-op (idempotent guard).
	if err := Seed(ctx, pool, log); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var jobs int
	_ = pool.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&jobs)
	if jobs != 13 {
		t.Errorf("re-seed changed job count: got %d, want 13", jobs)
	}
}
