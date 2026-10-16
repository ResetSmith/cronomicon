package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// A legacy placement takes its owner's work only (§0a, IN-6; the owner's yes
// to question 29). Until §0a it kept its 2.2 behaviour (MA-10): a runner that
// served two agencies before 2.3.0 is Global's, and claimed both agencies'
// runs. Now it claims neither (they wait for an agent of their own, and the
// notice legacy_placement says so), nor Global's, which it does not serve.
// Narrowed to one agency and handed to it (MA-12), it is that agency's agent
// and claims that agency's runs.
func TestALegacyPlacementClaimsOnlyItsOwnersRuns(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")
	caps := []string{"bash", "ansible"}
	insertRunner(t, svc, "r-legacy", "legacy", "online", caps) // owner: Global
	// Placed directly, as a pre-2.3.0 serve list was: the owner stays Global.
	for _, a := range []string{"a1", "a2"} {
		if _, err := svc.db.Exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r-legacy', ?)`, a); err != nil {
			t.Fatal(err)
		}
	}

	var owner string
	if err := svc.db.QueryRow(`SELECT owner_agency FROM runners WHERE id = 'r-legacy'`).Scan(&owner); err != nil || owner != "global" {
		t.Fatalf("fixture: owner = %q (%v), want global", owner, err)
	}
	try := func(runType, agency string) bool {
		t.Helper()
		trace := db.NewTraceID()
		insertQueuedRunAgency(t, svc, trace, runType, agency)
		got, err := svc.claimRun(ctx, "r-legacy", caps, true)
		if err != nil {
			t.Fatalf("claimRun: %v", err)
		}
		claimed := got != nil && got.TraceID == trace
		_, _ = svc.db.Exec(`DELETE FROM runs WHERE id = ?`, trace)
		return claimed
	}
	for _, runType := range []string{"bash", "ansible"} {
		for _, agency := range []string{"alpha", "beta", "Global"} {
			if try(runType, agency) {
				t.Errorf("a legacy placement owned by Global claimed %s's %s run", agency, runType)
			}
		}
	}
	// Narrowed to alpha and handed to it: alpha's agent, alpha's runs.
	if _, err := svc.db.Exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-legacy' AND agency_id = 'a2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE runners SET owner_agency = 'a1' WHERE id = 'r-legacy'`); err != nil {
		t.Fatal(err)
	}
	if !try("bash", "alpha") {
		t.Errorf("handed to alpha, the agent did not claim alpha's run")
	}
	if try("bash", "beta") {
		t.Errorf("handed to alpha, the agent claimed beta's run")
	}
}
