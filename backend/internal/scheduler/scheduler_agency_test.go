package scheduler

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestEnqueueRunSnapshotsAgencies verifies the enqueue snapshot after T3.6: the
// run's agency SET is frozen onto runs.agencies_json AND materialized into
// run_agencies (the migration-690 lookup structure claimRun probes). Both halves
// matter — a run with the snapshot but no index rows looks like general-pool work
// to the claim predicate and would be offered to the wrong runners.
func TestEnqueueRunSnapshotsAgencies(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "enqagency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	if err := EnqueueRun(ctx, pool, EnqueueParams{
		JobName: "tagged", RunType: "bash", Scope: "prod",
		TriggeredBy: "t", TriggerKind: "manual", Executor: "runner",
		AgenciesJSON: `["alpha","beta"]`,
	}); err != nil {
		t.Fatalf("EnqueueRun (tagged): %v", err)
	}
	if err := EnqueueRun(ctx, pool, EnqueueParams{
		JobName: "untagged", RunType: "bash", Scope: "prod",
		TriggeredBy: "t", TriggerKind: "manual", Executor: "runner",
	}); err != nil {
		t.Fatalf("EnqueueRun (untagged): %v", err)
	}

	var tagged, untagged string
	_ = pool.QueryRow(`SELECT agencies_json FROM runs WHERE job_name='tagged'`).Scan(&tagged)
	_ = pool.QueryRow(`SELECT agencies_json FROM runs WHERE job_name='untagged'`).Scan(&untagged)
	if tagged != `["alpha","beta"]` {
		t.Errorf("tagged run agencies_json = %q, want the full set", tagged)
	}
	// A general-pool run must be the EMPTY ARRAY, never NULL or "": the claim
	// predicate tests this column with a byte comparison against '[]'.
	if untagged != "[]" {
		t.Errorf("untagged run agencies_json = %q, want []", untagged)
	}

	var idx int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM run_agencies rag
	                   JOIN runs r ON r.id = rag.run_id WHERE r.job_name='tagged'`).Scan(&idx)
	if idx != 2 {
		t.Errorf("run_agencies rows for the tagged run = %d, want 2 — the snapshot was not materialized", idx)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM run_agencies rag
	                   JOIN runs r ON r.id = rag.run_id WHERE r.job_name='untagged'`).Scan(&idx)
	if idx != 0 {
		t.Errorf("run_agencies rows for the general-pool run = %d, want 0", idx)
	}
}
