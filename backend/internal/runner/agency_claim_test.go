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

// TestClaimRunAgencyMatrix is the M3 keystone test (agency-support.md §4.2): the
// disjoint hard-isolation matrix. A tagged run goes only to a runner in that
// agency; an untagged run goes only to a runner with NO agencies (the general
// pool). Backward-compat (no agencies anywhere) is already covered by the existing
// TestClaimRunTransition / TestCapabilityGuard, which pass unchanged.
func TestClaimRunAgencyMatrix(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")

	insertRunner(t, svc, "r-member", "member", "online", []string{"bash"})
	insertRunner(t, svc, "r-untagged", "untagged", "online", []string{"bash"})
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
		{"tagged → member runner", "alpha", "r-member", true},
		{"tagged → untagged runner", "alpha", "r-untagged", false},
		{"tagged → other-agency runner", "alpha", "r-other", false},
		{"untagged → untagged runner", "", "r-untagged", true},
		{"untagged → tagged runner (disjoint)", "", "r-member", false},
	}
	for _, c := range cases {
		if got := try(c.agency, c.runner); got != c.want {
			t.Errorf("%s: claimed=%v, want %v", c.name, got, c.want)
		}
	}
}
