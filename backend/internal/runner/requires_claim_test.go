package runner

import (
	"context"
	"testing"
)

// insertQueuedRunRequires inserts a queued runner run carrying a requires_json
// snapshot (Phase 4 claim-gating). agency "" ⇒ general pool.
func insertQueuedRunRequires(t *testing.T, svc *Service, traceID, runType, requiresJSON string) {
	t.Helper()
	var req any
	if requiresJSON != "" {
		req = requiresJSON
	}
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, requires_json, created_at)
		VALUES (?, 'j', ?, 'prod', 'queued', 'test', 'manual', 'runner', ?, ?)`,
		traceID, runType, req, now()); err != nil {
		t.Fatalf("insertQueuedRunRequires: %v", err)
	}
}

// TestClaimRunRequiresGating (Phase 4, §5): a run whose requires ⊄ the runner's
// capability tokens is NOT claimed; one whose requires ⊆ caps IS claimed; and a
// run with no requires (NULL / '[]') is claimed by any capability-matched runner
// (the pre-512 back-compat path).
func TestClaimRunRequiresGating(t *testing.T) {
	ctx := context.Background()

	t.Run("unsatisfied requires is not claimed", func(t *testing.T) {
		svc := newTestService(t)
		// Runner has ansible but NOT the vault capability.
		insertRunner(t, svc, "r1", "r1", "online", []string{"ansible", "checkout"})
		insertQueuedRunRequires(t, svc, "run-vault", "ansible", `["vault"]`)

		got, err := svc.claimRun(ctx, "r1", []string{"ansible", "checkout"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("run requiring vault must NOT be claimed by a vault-less runner, got %+v", got)
		}
	})

	t.Run("satisfied requires is claimed", func(t *testing.T) {
		svc := newTestService(t)
		caps := []string{"ansible", "checkout", "vault", "collection:community.vmware"}
		insertRunner(t, svc, "r1", "r1", "online", caps)
		insertQueuedRunRequires(t, svc, "run-ok", "ansible", `["vault","collection:community.vmware"]`)

		got, err := svc.claimRun(ctx, "r1", caps, true)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.TraceID != "run-ok" {
			t.Fatalf("run whose requires ⊆ caps must be claimed, got %+v", got)
		}
	})

	t.Run("no requires is claimed (back-compat)", func(t *testing.T) {
		svc := newTestService(t)
		insertRunner(t, svc, "r1", "r1", "online", []string{"ansible"})
		insertQueuedRunRequires(t, svc, "run-null", "ansible", "") // NULL requires_json

		got, err := svc.claimRun(ctx, "r1", []string{"ansible"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.TraceID != "run-null" {
			t.Fatalf("a run with NULL requires must be claimed by any type-matched runner, got %+v", got)
		}
	})

	t.Run("empty-array requires is claimed", func(t *testing.T) {
		svc := newTestService(t)
		insertRunner(t, svc, "r1", "r1", "online", []string{"ansible"})
		insertQueuedRunRequires(t, svc, "run-empty", "ansible", `[]`)

		got, err := svc.claimRun(ctx, "r1", []string{"ansible"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.TraceID != "run-empty" {
			t.Fatalf("a run with '[]' requires must be claimed, got %+v", got)
		}
	})

	t.Run("run_type still gates via json_each", func(t *testing.T) {
		svc := newTestService(t)
		// Runner only does bash; an ansible run must not be claimed.
		insertRunner(t, svc, "r1", "r1", "online", []string{"bash"})
		insertQueuedRunRequires(t, svc, "run-ans", "ansible", "")

		got, err := svc.claimRun(ctx, "r1", []string{"bash"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("run_type not in caps must not be claimed, got %+v", got)
		}
	})
}

// TestClaimRunRequiresAgencyComposes: capability-gating and agency-gating compose
// — a run that satisfies requires but is in a different agency is still refused.
func TestClaimRunRequiresAgencyComposes(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")
	caps := []string{"ansible", "vault"}
	insertRunner(t, svc, "r1", "r1", "online", caps)
	addRunnerAgency(t, svc, "r1", "a1") // r1 ∈ alpha

	// Run requires vault (satisfied) but is tagged beta (r1 is not in beta). The
	// agency snapshot is the SET (mig. 680) materialized into run_agencies (mig. 690)
	// — the scalar runs.agency was dropped in migration 700.
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, requires_json, created_at)
		VALUES ('run-beta', 'j', 'ansible', 'prod', 'queued', 'test', 'manual', 'runner', '["beta"]', '["vault"]', ?)`,
		now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO run_agencies(run_id, agency) VALUES('run-beta','beta')`); err != nil {
		t.Fatal(err)
	}
	got, err := svc.claimRun(ctx, "r1", caps, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("requires-satisfied run in a foreign agency must not be claimed, got %+v", got)
	}
}
