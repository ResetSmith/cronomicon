package sshexec

import (
	"context"
	"testing"
)

// TestSSHPoolClaimsPinnedRun is the second half of SB Phase 0's proof that the
// runner pin is unenforced on automatic producers (the first half,
// internal/scheduler's TestScheduledPinnedShellJobQueuesForSSH, shows such a run
// being queued for this pool).
//
// claim() carries no pin predicate, deliberately: the RT-1 note on its query
// says a pinned run is rejected upstream and never arrives. It does arrive, from
// every producer but the manual and token triggers, and this pool then runs it
// from the control plane — the outcome the pin was written to prevent.
//
// This test PASSES against the defect so the fix has something to turn around.
// The scope-bound-runners plan resolves it at executor resolution (Phase B) and
// retires the pin (Phase C); adding the predicate here is still the wrong fix,
// for the reason that note gives.
func TestSSHPoolClaimsPinnedRun(t *testing.T) {
	svc, pool := newReaperService(t)
	insertRun(t, pool, "run-pinned", "ssh", "queued", "")
	if _, err := pool.Exec(`UPDATE runs SET runner_tag = 'vlan-dmz' WHERE id = 'run-pinned'`); err != nil {
		t.Fatalf("pin run: %v", err)
	}

	r, err := svc.claim(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if r == nil || r.traceID != "run-pinned" {
		t.Fatalf("claim = %+v, want the pinned run (the pool does not look at runner_tag today)", r)
	}
	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id = 'run-pinned'`).Scan(&status); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running", status)
	}
}
