package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/settings"
)

// LR Phase 0 — today's behaviour, pinned before Phase G1 changes it.
//
// A run stores its agencies by NAME (runs.agencies_json, run_agencies.agency),
// while every membership table stores ids, and the claim matches a run to a
// runner through the live catalog: `rag.agency IN (SELECT a.name FROM
// runner_agencies ra JOIN agencies a …)`. Renaming an agency rewrites one row of
// `agencies` and nothing else, so a run queued under the old name is claimable
// by nobody — not its own agency's runner, whose name set now holds the new
// name, and not a general-pool runner, because the run is tagged. It waits
// until someone renames the agency back.
//
// This was the open item "Agency rename and queued runs" (§2.4): confirmed.
// Phase G1 fixes it by propagating the rename to queued and parked snapshots;
// when it does, the last assertion here inverts.
func TestLR0_RenamingAnAgencyStrandsItsQueuedRuns(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertAgencyRow(t, svc, "a-tax", "Tax")
	insertRunner(t, svc, "r-tax", "tax-runner", "online", []string{"bash"})
	insertRunner(t, svc, "r-pool", "pool-runner", "online", []string{"bash"})
	addRunnerAgency(t, svc, "r-tax", "a-tax")
	insertQueuedRunAgency(t, svc, "run-tax", "bash", "Tax")

	if _, err := settings.UpdateAgency(ctx, svc.db, "a-tax", settings.AgencyInput{Name: "Revenue"}, "root@example.com"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var snapshot, indexed string
	_ = svc.db.QueryRow(`SELECT agencies_json FROM runs WHERE id='run-tax'`).Scan(&snapshot)
	_ = svc.db.QueryRow(`SELECT agency FROM run_agencies WHERE run_id='run-tax'`).Scan(&indexed)
	if snapshot != `["Tax"]` || indexed != "Tax" {
		t.Fatalf("PIN: the rename reached the queued run (agencies_json=%s, run_agencies=%q). "+
			"If G1 now propagates a rename, replace this pin with its inverse.", snapshot, indexed)
	}

	for _, runner := range []string{"r-tax", "r-pool"} {
		got, err := svc.claimRun(ctx, runner, []string{"bash"}, true)
		if err != nil {
			t.Fatalf("claimRun(%s): %v", runner, err)
		}
		if got != nil {
			t.Errorf("PIN: %s claimed the run queued before the rename (G1 propagates the rename; "+
				"then its own agency's runner claims it and this pin inverts)", runner)
		}
	}
}
