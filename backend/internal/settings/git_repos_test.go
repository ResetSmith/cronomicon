package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// An agency with a repository cannot be deleted (GR-28): the repository's
// definitions, scripts, schedules and scopes are the agency's, and they go
// when the repository is disconnected, by the route that says how many.
func TestAnAgencyWithARepositoryCannotBeDeleted(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-lone', 'Lone', 't'), ('ag-none', 'None', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-lone', 'ag-lone', 'https://git.example.com/lone.git', 'main')`); err != nil {
		t.Fatal(err)
	}
	_, err := DeleteAgency(ctx, pool, "ag-lone", "root@example.com")
	var inUse *AgencyInUseError
	if !errors.As(err, &inUse) || inUse.Repository != 1 {
		t.Fatalf("deleting an agency that has a repository answered %v, want it refused for the repository", err)
	}
	if !strings.Contains(err.Error(), "1 connected repository (disconnect the agency's repository first)") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM agencies WHERE id = 'ag-lone'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the agency is gone (%d, %v)", n, err)
	}
	// One with nothing is deleted as before.
	if ok, err := DeleteAgency(ctx, pool, "ag-none", "root@example.com"); err != nil || !ok {
		t.Errorf("deleting an agency with no repository: %v, %v", ok, err)
	}
	// Its repository gone, the first can go too.
	if _, err := pool.Exec(`DELETE FROM git_repos WHERE id = 'repo-lone'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := DeleteAgency(ctx, pool, "ag-lone", "root@example.com"); err != nil || !ok {
		t.Errorf("deleting the agency once its repository is gone: %v, %v", ok, err)
	}
}
