package gitlab

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestWriteBranchResolution verifies the V1.1-10 branch-resolution precedence
// used by both sync (read) and publish (write): env override (via
// config.GitLabWriteBranch) → DB gitlab_config.write_branch → "main".
//
// A full publish-to-bare-repo fixture does not exist in this package, and the
// resolver is the only new branch-selection logic, so we test its precedence
// directly. The end-to-end push-to-branch behaviour (refspec wiring) is covered
// by manual/integration testing.
func TestWriteBranchResolution(t *testing.T) {
	ctx := context.Background()

	db := mustOpenDB(t)
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS gitlab_config (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    write_branch TEXT
);`); err != nil {
		t.Fatalf("create gitlab_config: %v", err)
	}

	// 1. No env, no DB row → default "main".
	s := &Service{db: db}
	if got := s.writeBranch(ctx); got != "main" {
		t.Fatalf("default: want %q, got %q", "main", got)
	}

	// 2. DB row set → DB value wins over default.
	if _, err := db.Exec(`INSERT INTO gitlab_config(id, write_branch) VALUES(1, 'release/test')`); err != nil {
		t.Fatalf("seed write_branch: %v", err)
	}
	if got := s.writeBranch(ctx); got != "release/test" {
		t.Fatalf("db value: want %q, got %q", "release/test", got)
	}

	// 3. Empty DB value → falls back to default.
	if _, err := db.Exec(`UPDATE gitlab_config SET write_branch='' WHERE id=1`); err != nil {
		t.Fatalf("clear write_branch: %v", err)
	}
	if got := s.writeBranch(ctx); got != "main" {
		t.Fatalf("empty db value: want %q, got %q", "main", got)
	}

	// 4. Env (config) override wins over DB even when DB is set.
	if _, err := db.Exec(`UPDATE gitlab_config SET write_branch='from-db' WHERE id=1`); err != nil {
		t.Fatalf("reset write_branch: %v", err)
	}
	s.Cfg = &config.Config{GitLabWriteBranch: "env-branch"}
	if got := s.writeBranch(ctx); got != "env-branch" {
		t.Fatalf("env override: want %q, got %q", "env-branch", got)
	}

	// 5. Nil db, no env → default "main" (no panic).
	s2 := &Service{}
	if got := s2.writeBranch(ctx); got != "main" {
		t.Fatalf("nil db: want %q, got %q", "main", got)
	}
}
