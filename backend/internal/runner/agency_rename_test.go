package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/settings"
)

// A run stores its agencies by NAME (runs.agencies_json, run_agencies.agency),
// while every membership table stores ids, and the claim matches a run to a
// runner through the live catalog. Until v2.3.0 renaming an agency rewrote one
// row of `agencies` and nothing else, so a run queued under the old name was
// claimable by nobody — not its own agency's runner, whose name set held the new
// name, and not a general-pool runner, because the run was tagged — until
// someone renamed the agency back. (First written as a pin of that, in Phase 0
// of the LR band.)
//
// The rename now carries onto every run that has not finished.
func TestRenamingAnAgencyCarriesOntoItsWaitingRuns(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertAgencyRow(t, svc, "a-tax", "Tax")
	insertAgencyRow(t, svc, "a-fin", "Finance")
	insertRunner(t, svc, "r-tax", "tax-runner", "online", []string{"bash"})
	insertRunner(t, svc, "r-pool", "pool-runner", "online", []string{"bash"})
	addRunnerAgency(t, svc, "r-tax", "a-tax")
	insertQueuedRunAgency(t, svc, "run-tax", "bash", "Tax")
	insertQueuedRunAgency(t, svc, "run-fin", "bash", "Finance")
	// A finished run keeps the name it ran under: history is not rewritten.
	insertQueuedRunAgency(t, svc, "run-old", "bash", "Tax")
	if _, err := svc.db.Exec(`UPDATE runs SET status = 'success' WHERE id = 'run-old'`); err != nil {
		t.Fatal(err)
	}
	// A parked run: the snapshot is a STRING holding JSON inside the frozen
	// parameters, beside keys the rename must not disturb.
	params, _ := json.Marshal(map[string]any{"JobName": "j", "Executor": "runner", "AgenciesJSON": `["Tax"]`, "Priority": 3})
	if _, err := svc.db.Exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, params_json)
	                          VALUES ('p-tax','job','j','cronomicon','prod','2099-01-01T00:00:00Z','someone',?,?)`, now(), string(params)); err != nil {
		t.Fatal(err)
	}

	if _, err := settings.UpdateAgency(ctx, svc.db, "a-tax", settings.AgencyInput{Name: "Revenue"}, "root@example.com"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	snapshot := func(run string) (aj, idx string) {
		t.Helper()
		_ = svc.db.QueryRow(`SELECT agencies_json FROM runs WHERE id = ?`, run).Scan(&aj)
		_ = svc.db.QueryRow(`SELECT COALESCE(group_concat(agency), '') FROM run_agencies WHERE run_id = ?`, run).Scan(&idx)
		return aj, idx
	}
	if aj, idx := snapshot("run-tax"); aj != `["Revenue"]` || idx != "Revenue" {
		t.Errorf("the queued run still carries the old name: agencies_json=%s run_agencies=%q", aj, idx)
	}
	if aj, idx := snapshot("run-fin"); aj != `["Finance"]` || idx != "Finance" {
		t.Errorf("another agency's run was touched: agencies_json=%s run_agencies=%q", aj, idx)
	}
	if aj, idx := snapshot("run-old"); aj != `["Tax"]` || idx != "Tax" {
		t.Errorf("a finished run's history was rewritten: agencies_json=%s run_agencies=%q", aj, idx)
	}
	var parked string
	_ = svc.db.QueryRow(`SELECT params_json FROM pending_runs WHERE id = 'p-tax'`).Scan(&parked)
	var got map[string]any
	if err := json.Unmarshal([]byte(parked), &got); err != nil {
		t.Fatalf("the parked parameters are no longer JSON: %v (%s)", err, parked)
	}
	if got["AgenciesJSON"] != `["Revenue"]` || got["JobName"] != "j" || got["Executor"] != "runner" || got["Priority"] != float64(3) {
		t.Errorf("the parked snapshot after the rename = %s", parked)
	}

	// And the point of it: the agency's own runner claims the run; nobody else does.
	if got, err := svc.claimRun(ctx, "r-pool", []string{"bash"}, true); err != nil || got != nil {
		t.Errorf("a general-pool runner claimed a tagged run after the rename (%v, %v)", got, err)
	}
	got1, err := svc.claimRun(ctx, "r-tax", []string{"bash"}, true)
	if err != nil || got1 == nil || got1.TraceID != "run-tax" {
		t.Errorf("the renamed agency's runner did not claim its queued run (%v, %v)", got1, err)
	}
}
