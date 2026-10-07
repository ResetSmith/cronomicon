package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// MA-10 — a legacy placement keeps its 2.2 behaviour. A runner that served two
// agencies before 2.3.0 is Global's now and still claims both agencies' runs,
// Ansible included, and still none of Global's: who administers it changed, and
// nothing about what it runs.
func TestALegacyPlacementStillClaimsItsAgenciesRuns(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")
	caps := []string{"bash", "ansible"}
	insertRunner(t, svc, "r-legacy", "legacy", "online", caps) // owner: Global
	addRunnerAgency(t, svc, "r-legacy", "a1")
	addRunnerAgency(t, svc, "r-legacy", "a2")

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
		for _, agency := range []string{"alpha", "beta"} {
			if !try(runType, agency) {
				t.Errorf("a legacy placement did not claim %s's %s run", agency, runType)
			}
		}
		// It is owned by Global and does not SERVE Global: ownership is who
		// administers a runner, never what it claims.
		if try(runType, "Global") {
			t.Errorf("a runner owned by Global and serving two agencies claimed Global's %s run", runType)
		}
	}
}
