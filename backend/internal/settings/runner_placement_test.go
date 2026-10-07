package settings

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// MA-11, the invariant itself: for an agent, the list after a write is exactly
// the owner, or a non-empty subset of the list before it.
func TestCheckRunnerPlacement(t *testing.T) {
	for _, tc := range []struct {
		name          string
		local         bool
		owner         string
		before, after []string
		want          error
	}{
		{"an agent keeps its one agency", false, "ag-a", []string{"ag-a"}, []string{"ag-a"}, nil},
		{"an agent cannot gain an agency", false, "ag-a", []string{"ag-a"}, []string{"ag-a", "ag-b"}, ErrServeListFixed},
		{"an agent cannot swap its agency", false, "ag-a", []string{"ag-a"}, []string{"ag-b"}, ErrServeListFixed},
		{"an agent cannot be moved to Global", false, "ag-a", []string{"ag-a"}, []string{"global"}, ErrServeListFixed},
		{"an agent's list cannot be emptied", false, "ag-a", []string{"ag-a"}, nil, ErrAgencyRequired},
		{"a Global agent keeps Global", false, "global", []string{"global"}, []string{"global"}, nil},
		{"a Global agent cannot be given an agency", false, "global", []string{"global"}, []string{"ag-a"}, ErrServeListFixed},
		{"a legacy placement is left as it is", false, "global", []string{"ag-a", "ag-b"}, []string{"ag-b", "ag-a"}, nil},
		{"a legacy placement can lose an agency", false, "global", []string{"ag-a", "ag-b"}, []string{"ag-a"}, nil},
		{"a legacy placement cannot gain one", false, "global", []string{"ag-a", "ag-b"}, []string{"ag-a", "ag-b", "ag-c"}, ErrServeListFixed},
		{"a legacy placement cannot trade one", false, "global", []string{"ag-a", "ag-b"}, []string{"ag-a", "ag-c"}, ErrServeListFixed},
		{"a legacy placement cannot be emptied", false, "global", []string{"ag-a", "ag-b"}, []string{}, ErrAgencyRequired},
		{"a legacy placement may be settled as its owner's", false, "global", []string{"ag-a", "ag-b"}, []string{"global"}, nil},
		{"a repeated id is one id", false, "ag-a", []string{"ag-a"}, []string{"ag-a", "ag-a"}, nil},
		{"the local runner takes any non-empty list", true, "global", []string{"global"}, []string{"ag-a", "ag-b"}, nil},
		{"the local runner's list cannot be emptied", true, "global", []string{"global"}, nil, ErrAgencyRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckRunnerPlacement(tc.local, tc.owner, tc.before, tc.after); !errors.Is(err, tc.want) {
				t.Errorf("CheckRunnerPlacement(owner %s, %v -> %v) = %v, want %v", tc.owner, tc.before, tc.after, err, tc.want)
			}
		})
	}
}

func placementDB(t *testing.T) *sql.DB {
	t.Helper()
	pool := membershipDB(t) // agencies ag-dss, ag-nwd
	for _, q := range []string{
		// dss-agent: DSS's own. global-agent: Global's own. legacy: Global's,
		// serving both agencies since before 2.3.0.
		`INSERT INTO runners(id,name,status,capabilities,registered_at,created_at,owner_agency)
		 VALUES('r-dss','dss-agent','online','["bash"]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','ag-dss')`,
		`INSERT INTO runners(id,name,status,capabilities,registered_at,created_at)
		 VALUES('r-global','global-agent','online','["bash"]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO runners(id,name,status,capabilities,registered_at,created_at)
		 VALUES('r-legacy','legacy','online','["bash"]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r-legacy','ag-dss'), ('r-legacy','ag-nwd')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	return pool
}

func serveList(t *testing.T, pool *sql.DB, runnerID string) []string {
	t.Helper()
	rows, err := pool.Query(`SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id`, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func ownerOf(t *testing.T, pool *sql.DB, runnerID string) string {
	t.Helper()
	var o string
	if err := pool.QueryRow(`SELECT owner_agency FROM runners WHERE id = ?`, runnerID).Scan(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

// A runner is born serving its owner (the trigger of migration 1250), and the
// fixture above is what it claims to be.
func TestARunnerIsBornServingItsOwner(t *testing.T) {
	pool := placementDB(t)
	for id, want := range map[string][]string{
		"r-dss":    {"ag-dss"},
		"r-global": {"global"},
		"r-legacy": {"ag-dss", "ag-nwd"},
	} {
		if got := serveList(t, pool, id); !slices.Equal(got, want) {
			t.Errorf("%s serves %v, want %v", id, got, want)
		}
	}
	if _, err := pool.Exec(`UPDATE runners SET owner_agency = '' WHERE id = 'r-dss'`); err == nil {
		t.Error("a runner was left with no owner")
	}
}

// Nobody can add an agency to an agent's serve list, through either writer, and
// neither writer can empty one. A refused write changes nothing.
func TestNoWriterWidensOrEmptiesAnAgentsServeList(t *testing.T) {
	pool := placementDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		runner string
		to     []string
		want   error
	}{
		{"r-dss", []string{"ag-dss", "ag-nwd"}, ErrServeListFixed},
		{"r-dss", []string{"ag-nwd"}, ErrServeListFixed},
		{"r-dss", []string{"global"}, ErrServeListFixed},
		{"r-dss", nil, ErrAgencyRequired},
		{"r-global", []string{"ag-dss"}, ErrServeListFixed},
		{"r-global", nil, ErrAgencyRequired},
	} {
		before := serveList(t, pool, tc.runner)
		err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: tc.runner, AgencyIDs: tc.to}}, "t@example.com")
		if !errors.Is(err, tc.want) {
			t.Errorf("SetRunnerAgencies(%s -> %v) = %v, want %v", tc.runner, tc.to, err, tc.want)
		}
		if got := serveList(t, pool, tc.runner); !slices.Equal(got, before) {
			t.Errorf("a refused write changed %s's serve list: %v -> %v", tc.runner, before, got)
		}
	}
	// The same through the agency's member list: NWD cannot take DSS's agent or
	// Global's, and DSS cannot drop its own agent (that would leave it serving
	// nobody).
	for _, r := range []string{"r-dss", "r-global"} {
		if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", refs([2]string{"runner", r}, [2]string{"runner", "r-legacy"}), "t@example.com"); !errors.Is(err, ErrServeListFixed) {
			t.Errorf("adding %s to NWD's members = %v, want ErrServeListFixed", r, err)
		}
	}
	// (The owner rule that already guards secrets, variables and keys answers
	// first, now that a runner has an owner too: an owned row is moved, not
	// removed. The invariant behind it would refuse the same write.)
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", refs([2]string{"runner", "r-legacy"}), "t@example.com"); !errors.Is(err, ErrOwnerRemoval) {
		t.Errorf("removing DSS's own agent from DSS = %v, want ErrOwnerRemoval", err)
	}
	if got := serveList(t, pool, "r-dss"); !slices.Equal(got, []string{"ag-dss"}) {
		t.Errorf("r-dss serves %v after the refused writes, want [ag-dss]", got)
	}
	// An unchanged list is accepted: the matrix posts every runner it shows.
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: "r-dss", AgencyIDs: []string{"ag-dss"}}}, "t@example.com"); err != nil {
		t.Errorf("re-posting an agent's own list: %v", err)
	}
}

// A legacy placement can lose an agency and never gain one, through either
// writer; and what it lost it cannot have back.
func TestALegacyPlacementOnlyNarrows(t *testing.T) {
	pool := placementDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-new','NEW','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{
		{RunnerID: "r-legacy", AgencyIDs: []string{"ag-dss", "ag-nwd", "ag-new"}},
	}, "t@example.com"); !errors.Is(err, ErrServeListFixed) {
		t.Fatalf("widening a legacy placement = %v, want ErrServeListFixed", err)
	}
	if _, err := SetAgencyMembers(ctx, pool, "ag-new", refs([2]string{"runner", "r-legacy"}), "t@example.com"); !errors.Is(err, ErrServeListFixed) {
		t.Fatalf("adding a legacy placement to another agency = %v, want ErrServeListFixed", err)
	}
	// Narrowed through the agency's list: NWD drops it.
	if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", nil, "t@example.com"); err != nil {
		t.Fatalf("taking a legacy placement off NWD: %v", err)
	}
	if got := serveList(t, pool, "r-legacy"); !slices.Equal(got, []string{"ag-dss"}) {
		t.Fatalf("after narrowing it serves %v, want [ag-dss]", got)
	}
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{
		{RunnerID: "r-legacy", AgencyIDs: []string{"ag-dss", "ag-nwd"}},
	}, "t@example.com"); !errors.Is(err, ErrServeListFixed) {
		t.Fatalf("putting NWD back = %v, want ErrServeListFixed", err)
	}
	// It is still a legacy placement: Global's, serving DSS.
	if o := ownerOf(t, pool, "r-legacy"); o != "global" || !IsLegacyPlacement(o, serveList(t, pool, "r-legacy")) {
		t.Errorf("owner %q serving %v should read as a legacy placement", o, serveList(t, pool, "r-legacy"))
	}
}

// MA-12: the one owner change. Global to the single agency the agent serves is
// accepted; everything else is refused and changes nothing.
func TestSetRunnerOwner(t *testing.T) {
	pool := placementDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name, runner, to string
	}{
		{"agency to Global", "r-dss", "global"},
		{"agency to agency", "r-dss", "ag-nwd"},
		{"Global's own agent to an agency it does not serve", "r-global", "ag-dss"},
		{"a legacy placement that still serves two", "r-legacy", "ag-dss"},
	} {
		before := ownerOf(t, pool, tc.runner)
		if err := SetRunnerOwner(ctx, pool, tc.runner, tc.to, "t@example.com"); !errors.Is(err, ErrOwnerChangeRefused) {
			t.Errorf("%s = %v, want ErrOwnerChangeRefused", tc.name, err)
		}
		if got := ownerOf(t, pool, tc.runner); got != before {
			t.Errorf("%s: a refused change moved the owner %q -> %q", tc.name, before, got)
		}
	}
	if err := SetRunnerOwner(ctx, pool, "no-such-runner", "ag-dss", "t@example.com"); !errors.Is(err, ErrUnknownRunner) {
		t.Errorf("an unknown runner = %v, want ErrUnknownRunner", err)
	}

	// Narrow the legacy placement to DSS, then hand it over.
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: "r-legacy", AgencyIDs: []string{"ag-dss"}}}, "t@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := SetRunnerOwner(ctx, pool, "r-legacy", "ag-nwd", "t@example.com"); !errors.Is(err, ErrOwnerChangeRefused) {
		t.Errorf("handing it to an agency it does not serve = %v, want ErrOwnerChangeRefused", err)
	}
	if err := SetRunnerOwner(ctx, pool, "r-legacy", "ag-dss", "alice@example.com"); err != nil {
		t.Fatalf("Global to the one served agency: %v", err)
	}
	if o := ownerOf(t, pool, "r-legacy"); o != "ag-dss" || IsLegacyPlacement(o, serveList(t, pool, "r-legacy")) {
		t.Errorf("after the hand-over: owner %q serving %v, want DSS's own agent", o, serveList(t, pool, "r-legacy"))
	}
	var summary string
	if err := pool.QueryRow(`SELECT actor || '|' || summary FROM activity WHERE runner_name = 'legacy' ORDER BY id DESC LIMIT 1`).Scan(&summary); err != nil {
		t.Fatalf("the hand-over wrote no activity row: %v", err)
	}
	if summary != "alice@example.com|handed to DSS: it was Global's and served only that agency; its administrators manage it now" {
		t.Errorf("activity row = %q", summary)
	}
	// Asking again is a no-op, and it cannot be taken back.
	if err := SetRunnerOwner(ctx, pool, "r-legacy", "ag-dss", "t@example.com"); err != nil {
		t.Errorf("repeating the hand-over: %v", err)
	}
	if err := SetRunnerOwner(ctx, pool, "r-legacy", "global", "t@example.com"); !errors.Is(err, ErrOwnerChangeRefused) {
		t.Errorf("taking it back to Global = %v, want ErrOwnerChangeRefused", err)
	}
}
