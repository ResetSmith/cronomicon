package execspec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestAgencyHasOnlineRunner covers the M4 eligibility helper across the disjoint
// matrix: a named agency needs an ONLINE member; the empty agency (general pool)
// needs an online runner with no agencies.
func TestAgencyHasOnlineRunner(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "agencyonline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a1','alpha','t')`)
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a2','beta','t')`)
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('r1','member','online','t','t')`)
	exec(`INSERT INTO runner_agencies(runner_id,agency_id) VALUES('r1','a1')`) // online member of alpha
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('r2','off','offline','t','t')`)
	exec(`INSERT INTO runner_agencies(runner_id,agency_id) VALUES('r2','a2')`)                               // offline member of beta
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('r3','gen','online','t','t')`) // online, untagged

	ctx := context.Background()
	// T3.6 — the helper now takes the run's agency SET. An EMPTY set is the general
	// pool (unchanged, AG-Q3a); a non-empty set is satisfied by an online member of
	// ANY of its agencies.
	check := func(want bool, agencies ...string) {
		t.Helper()
		got, err := AgenciesHaveOnlineRunner(ctx, pool, agencies)
		if err != nil {
			t.Fatalf("AgenciesHaveOnlineRunner(%v): %v", agencies, err)
		}
		if got != want {
			t.Errorf("AgenciesHaveOnlineRunner(%v) = %v, want %v", agencies, got, want)
		}
	}
	check(true, "alpha")  // an online member exists
	check(false, "beta")  // its only member is offline
	check(false, "gamma") // no such agency
	check(true)           // empty set ⇒ general pool, and an online untagged runner exists

	// The property the set form adds: ANY member of ANY listed agency suffices, so a
	// run requiring beta-or-alpha is servable even though beta alone is not.
	check(true, "beta", "alpha")
	check(false, "beta", "gamma")

	// Tag the last general runner → no online untagged runner remains.
	exec(`INSERT INTO runner_agencies(runner_id,agency_id) VALUES('r3','a1')`)
	check(false)
}

// TestEligibleOnlineRunnerForRun (Phase 4, §5): the requirements-aware stuck-run
// probe — a run is eligible only if some online, agency-eligible runner's
// capability tokens cover both its run_type and its requires set.
func TestEligibleOnlineRunnerForRun(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "eligible.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// General-pool runner with ansible + checkout but NOT vault.
	exec(`INSERT INTO runners(id,name,status,capabilities,registered_at,created_at)
	      VALUES('r1','m','online','["ansible","checkout"]','t','t')`)

	mkRun := func(id, runType, requires string) {
		var req any
		if requires != "" {
			req = requires
		}
		exec(`INSERT INTO runs(id,job_name,run_type,scope,status,triggered_by,trigger_kind,executor,requires_json,created_at)
		      VALUES(?, 'j', ?, 'prod', 'queued', 't', 'manual', 'runner', ?, 't')`, id, runType, req)
	}
	mkRun("ok", "ansible", `["checkout"]`)   // satisfied
	mkRun("novault", "ansible", `["vault"]`) // requires vault → no eligible runner
	mkRun("bare", "ansible", "")             // no requires → eligible
	mkRun("wrongtype", "terraform", "")      // run_type not in caps → not eligible

	ctx := context.Background()
	check := func(id string, wantOK bool, wantReq []string) {
		t.Helper()
		ok, requires, err := EligibleOnlineRunnerForRun(ctx, pool, id)
		if err != nil {
			t.Fatalf("EligibleOnlineRunnerForRun(%q): %v", id, err)
		}
		if ok != wantOK {
			t.Errorf("EligibleOnlineRunnerForRun(%q) ok = %v, want %v", id, ok, wantOK)
		}
		if len(requires) != len(wantReq) {
			t.Errorf("EligibleOnlineRunnerForRun(%q) requires = %v, want %v", id, requires, wantReq)
		}
	}
	check("ok", true, []string{"checkout"})
	check("novault", false, []string{"vault"}) // returns the unmet requirement for the hint
	check("bare", true, nil)
	check("wrongtype", false, nil)
}
