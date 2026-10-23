package gitlab

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestWriteBranchResolution verifies the V1.1-10 branch-resolution precedence
// used by both sync (read) and publish (write): env override (via
// config.GitLabWriteBranch, Global's only) → the repository's git_repos.branch → "main".
//
// A full publish-to-bare-repo fixture does not exist in this package, and the
// resolver is the only new branch-selection logic, so we test its precedence
// directly. The end-to-end push-to-branch behaviour (refspec wiring) is covered
// by manual/integration testing.
func TestWriteBranchResolution(t *testing.T) {
	ctx := context.Background()

	db := mustOpenDB(t)

	// 1. No env, nothing chosen in the row → default "main".
	s := &Service{db: db}
	if got := s.writeBranch(ctx); got != "main" {
		t.Fatalf("default: want %q, got %q", "main", got)
	}

	// 2. DB row set → DB value wins over default.
	if _, err := db.Exec(`UPDATE git_repos SET branch='release/test' WHERE id='global'`); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	if got := s.writeBranch(ctx); got != "release/test" {
		t.Fatalf("db value: want %q, got %q", "release/test", got)
	}

	// 3. Empty DB value → falls back to default.
	if _, err := db.Exec(`UPDATE git_repos SET branch='' WHERE id='global'`); err != nil {
		t.Fatalf("clear branch: %v", err)
	}
	if got := s.writeBranch(ctx); got != "main" {
		t.Fatalf("empty db value: want %q, got %q", "main", got)
	}

	// 4. Env (config) override wins over DB even when DB is set.
	if _, err := db.Exec(`UPDATE git_repos SET branch='from-db' WHERE id='global'`); err != nil {
		t.Fatalf("reset branch: %v", err)
	}
	s.Cfg = &config.Config{GitLabWriteBranch: "env-branch"}
	if got := s.writeBranch(ctx); got != "env-branch" {
		t.Fatalf("env override: want %q, got %q", "env-branch", got)
	}

	// 5. The override is Global's repository's alone (GR-21): another
	// repository's Service reads its own row, and "main" when it names none.
	if _, err := db.Exec(`INSERT INTO git_repos(id, agency_id, branch) VALUES('repo-b', 'ag-b', 'b-branch')`); err != nil {
		t.Fatalf("second repository: %v", err)
	}
	other := &Service{db: db, repoID: "repo-b", agencyID: "ag-b", Cfg: s.Cfg}
	if got := other.writeBranch(ctx); got != "b-branch" {
		t.Fatalf("another repository: want its own row's %q, got %q", "b-branch", got)
	}
	if _, err := db.Exec(`UPDATE git_repos SET branch='' WHERE id='repo-b'`); err != nil {
		t.Fatalf("clear the second repository's branch: %v", err)
	}
	if got := other.writeBranch(ctx); got != "main" {
		t.Fatalf("another repository with no branch: want %q, got %q", "main", got)
	}

	// 5. Nil db, no env → default "main" (no panic).
	s2 := &Service{}
	if got := s2.writeBranch(ctx); got != "main" {
		t.Fatalf("nil db: want %q, got %q", "main", got)
	}
}
