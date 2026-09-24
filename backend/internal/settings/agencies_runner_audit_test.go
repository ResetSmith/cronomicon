package settings

import (
	"context"
	"testing"
)

// DRF-5: a manual placement is an isolation change, so the feed row it writes
// has to be findable by runner and has to say what the runner is now in. The
// previous row said "membership-updated runner agencies" with no runner name —
// present, and useless.
func TestSetRunnerAgenciesWritesAUsefulActivityRow(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-carson','Carson','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-reno','Reno','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	      VALUES ('r1','ansible-rh8','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	      VALUES ('r2','untouched','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r2','ag-reno')`)

	rowsFor := func(name string) []string {
		t.Helper()
		rows, err := database.Query(
			`SELECT actor || '|' || summary FROM activity WHERE runner_name=? ORDER BY id`, name)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}

	// One request that moves r1 and re-posts r2 unchanged — the matrix UI's
	// shape. Only r1 may write a row.
	if err := SetRunnerAgencies(ctx, database, []RunnerAgencies{
		{RunnerID: "r1", AgencyIDs: []string{"ag-carson", "ag-reno"}},
		{RunnerID: "r2", AgencyIDs: []string{"ag-reno"}},
	}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := rowsFor("ansible-rh8"); len(got) != 1 || got[0] != "alice@example.com|agencies set: Carson, Reno" {
		t.Errorf("placed runner: got %v", got)
	}
	if got := rowsFor("untouched"); len(got) != 0 {
		t.Errorf("an unchanged runner must write nothing, got %v", got)
	}

	// Clearing to the general pool says so — RB-22's rule that the empty set is
	// stated, not implied.
	if err := SetRunnerAgencies(ctx, database, []RunnerAgencies{
		{RunnerID: "r1", AgencyIDs: nil},
	}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := rowsFor("ansible-rh8"); len(got) != 2 || got[1] != "alice@example.com|removed from all agencies (now general pool)" {
		t.Errorf("clearing: got %v", got)
	}

	// The Change Log keeps its request-level row, now carrying details.
	var details string
	if err := database.QueryRow(
		`SELECT details FROM change_log WHERE action='membership-updated' ORDER BY id DESC LIMIT 1`,
	).Scan(&details); err != nil {
		t.Fatalf("change_log row: %v", err)
	}
	if details == "" {
		t.Error("change_log details should name what moved")
	}
}
