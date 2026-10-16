package notices_test

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An agency with no repository connected is reported to it, Global included
// when its connection has no URL and the environment names none (§0a, IN-2;
// Phase I3; question 39). It clears when the repository is connected.
func TestI3_AnAgencyWithNoRepositoryIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-tax', 'ag-tax', 'https://git.example.com/tax.git', 'main')`)
	mustExec(t, pool, `INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('global', 'global', '', 'main')`)
	mustExec(t, pool, `UPDATE git_repos SET url = '' WHERE id = 'global'`)
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindAgencyNoRepository)
	}

	notices.SetGlobalRepoFromEnvironment(false)
	t.Cleanup(func() { notices.SetGlobalRepoFromEnvironment(false) })
	got := check()
	if len(got) != 2 || got["ag-fin"].AgencyID != "ag-fin" || got["global"].AgencyID != "global" {
		t.Fatalf("agency_no_repository = %v, want Finance's and Global's", got)
	}

	// Global's URL from the environment, and Finance connecting its own.
	notices.SetGlobalRepoFromEnvironment(true)
	mustExec(t, pool, `INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'https://git.example.com/fin.git', 'main')`)
	if got = check(); len(got) != 0 {
		t.Errorf("agency_no_repository after both were connected = %v, want none", got)
	}
}
