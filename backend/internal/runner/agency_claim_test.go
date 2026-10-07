package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

func insertAgencyRow(t *testing.T, svc *Service, id, name string) {
	t.Helper()
	if _, err := svc.db.Exec(`INSERT INTO agencies(id, name, created_at) VALUES(?,?,?)`, id, name, now()); err != nil {
		t.Fatalf("insertAgencyRow: %v", err)
	}
}

func addRunnerAgency(t *testing.T, svc *Service, runnerID, agencyID string) {
	t.Helper()
	if _, err := svc.db.Exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES(?,?)`, runnerID, agencyID); err != nil {
		t.Fatalf("addRunnerAgency: %v", err)
	}
}

func insertQueuedRunAgency(t *testing.T, svc *Service, traceID, runType, agency string) {
	t.Helper()
	// Phase 3 — the claim predicate reads the agency SET (agencies_json, materialized
	// into run_agencies). The scalar runs.agency it replaced was dropped in migration
	// 700, so this fixture writes exactly what the enqueue path writes.
	aj := "[]"
	if agency != "" {
		b, _ := json.Marshal([]string{agency})
		aj = string(b)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		VALUES (?, 'j', ?, 'prod', 'queued', 'test', 'manual', 'runner', ?, ?)`,
		traceID, runType, aj, now()); err != nil {
		t.Fatalf("insertQueuedRunAgency: %v", err)
	}
	if agency != "" {
		if _, err := svc.db.Exec(`INSERT INTO run_agencies(run_id, agency) VALUES(?,?)`, traceID, agency); err != nil {
			t.Fatalf("insertQueuedRunAgency: materialize: %v", err)
		}
	}
}

// TestClaimRunAgencyMatrix is the isolation matrix (M3, agency-support.md §4.2;
// one arm since migration 1220): a runner claims a run only when it serves one
// of the run's agencies. Global is an agency like the others here — a runner
// that serves Global claims Global's runs and no department's, and a
// department's runner never claims Global's. (Until 2.3.0 the Global row was
// "no row", on both sides, and the rule had a second arm for it.)
func TestClaimRunAgencyMatrix(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")

	insertRunner(t, svc, "r-member", "member", "online", []string{"bash"})
	insertRunner(t, svc, "r-global", "global", "online", []string{"bash"}) // born serving Global
	insertRunner(t, svc, "r-other", "other", "online", []string{"bash"})
	addRunnerAgency(t, svc, "r-member", "a1") // member of alpha
	addRunnerAgency(t, svc, "r-other", "a2")  // member of beta only

	// try inserts one queued run with the given agency, attempts a claim by runnerID,
	// reports whether THAT run was claimed, and removes it so cases don't bleed.
	try := func(agency, runnerID string) bool {
		trace := db.NewTraceID()
		insertQueuedRunAgency(t, svc, trace, "bash", agency)
		got, err := svc.claimRun(ctx, runnerID, []string{"bash"}, true)
		if err != nil {
			t.Fatalf("claimRun: %v", err)
		}
		claimed := got != nil && got.TraceID == trace
		_, _ = svc.db.Exec(`DELETE FROM runs WHERE id=?`, trace)
		return claimed
	}

	cases := []struct {
		name, agency, runner string
		want                 bool
	}{
		{"alpha's run → alpha's runner", "alpha", "r-member", true},
		{"alpha's run → Global's runner", "alpha", "r-global", false},
		{"alpha's run → beta's runner", "alpha", "r-other", false},
		{"Global's run → Global's runner", "", "r-global", true},
		{"Global's run → alpha's runner", "", "r-member", false},
	}
	for _, c := range cases {
		if got := try(c.agency, c.runner); got != c.want {
			t.Errorf("%s: claimed=%v, want %v", c.name, got, c.want)
		}
	}
}
