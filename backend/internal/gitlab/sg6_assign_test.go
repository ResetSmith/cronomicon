package gitlab

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// 2.5.1 SG6.4 (the pre-release review's A9): an assignment into an agency
// whose repository row has no URL (Global's, before 2.5.3, on an installation that never
// connected it, an agency from before 2.4.0) reached the commit and answered
// 409 target_unsynced "assign again" forever, since its sync only ever
// records "not configured". It is 422 no_repository, as a publish is since
// 0f23181, and as an agency with no row at all already was.
func TestSG6_AnAssignmentToAnUnconnectedRepositoryIsRefused(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	ctx := context.Background()

	// No row at all: an agency created in 2.5.1 without a repository.
	grAgencyScope(t, f.origin.db, "ag-bare", "hosts-of-bare")
	_, err := f.reg.Assign(ctx, AssignRequest{Kind: "job", UID: f.jobUID, TargetAgency: "ag-bare", Scope: "hosts-of-bare", Actor: "admin@example.com"})
	if refusalCode(err) != "no_repository" {
		t.Errorf("into an agency with no repository row: code %q (%v), want no_repository", refusalCode(err), err)
	}

	// A row with no URL, its Service running as the registry builds it.
	grAgencyScope(t, f.origin.db, "ag-old", "hosts-of-old")
	if _, err := f.origin.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-old', 'ag-old', '', ?)`, f.branch); err != nil {
		t.Fatal(err)
	}
	// What the copy's schedules name, so the assignment gets as far as the commit.
	if _, err := f.origin.db.Exec(`INSERT INTO calendars (source, name, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 't', 't', 'ag-old')`); err != nil {
		t.Fatal(err)
	}
	f.reg.services["repo-old"] = &Service{db: f.origin.db, log: slog.New(slog.NewTextHandler(io.Discard, nil)), repoURL: "", repoID: "repo-old",
		agencyID: "ag-old", cloneDir: filepath.Join(t.TempDir(), "clone"), Cfg: &config.Config{GitLabWriteBranch: f.branch}}
	_, err = f.reg.Assign(ctx, AssignRequest{Kind: "job", UID: f.jobUID, TargetAgency: "ag-old", Scope: "hosts-of-old", Actor: "admin@example.com"})
	if refusalCode(err) != "no_repository" {
		t.Errorf("into an agency whose repository has no URL: code %q (%v), want no_repository", refusalCode(err), err)
	}
	var n int
	if err := f.origin.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE owner_agency = 'ag-old'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the refused assignment left %d jobs in ag-old (%v)", n, err)
	}
}
