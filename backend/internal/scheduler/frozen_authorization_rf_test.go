package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// RF-11 — RB-Q12's posture, asserted where it actually happens
// (the RBAC-fixes plan).
//
// The API-side test of the same name only ever checked that creation is gated and
// that the bound scope is frozen onto the row. Neither half is the decision:
// RB-Q12 chose that a pending run fires with the authorization it was created
// under, so **revoking a creator's grants does not cancel their parked runs**.
// That property lives in PromotePending, which attaches no identity and
// re-resolves nothing — and until now nothing asserted it, in either package.
//
// It is written as a test for the same reason RB-Q11(c) is: the behavior looks
// like an oversight to anyone who meets it cold, so a later "fix" should have to
// argue with a test that states the decision rather than silently flip a posture
// the product owner chose.

// TestRevokedCreatorsPendingRunStillFires — the RB-Q12 decision, end to end.
//
// A run is parked by a user who, at that moment, holds every grant it needs. The
// grants are then deleted outright — the harshest form of revocation, an
// offboarding rather than a role edit. Promotion still fires the run, because
// promotion replays frozen params and never asks who the creator is now.
//
// The accepted consequence, stated plainly: recourse is the manual sweep — find
// the parked rows by creator and cancel them — which is why scheduled_by is on
// the row and why RB-4's pre-flight lists pending runs by creator.
func TestRevokedCreatorsPendingRunStillFires(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	seedPendingJob(t, pool)

	// The creator's authorization, as it stood when the run was parked.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		VALUES ('g-op','sg-ops','operator',NULL,1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	id := insertPending(t, pool, time.Now().Add(-time.Minute)) // due

	// Offboarding: every grant this user had, gone.
	if _, err := pool.ExecContext(ctx, `DELETE FROM access_grants WHERE ad_group = 'sg-ops'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	s := scheduler.New(pool, discardLog(), nil)
	s.PromotePending(ctx)

	var status string
	if err := pool.QueryRowContext(ctx,
		`SELECT status FROM runs WHERE job_name = 'deferred-job'`).Scan(&status); err != nil {
		t.Fatalf("the parked run did NOT fire after its creator's grants were revoked — that is "+
			"the re-check-at-promotion behavior RB-Q12 REJECTED; if this is now wanted it is a "+
			"decision to make deliberately, not a bug to fix quietly: %v", err)
	}
	if status != "queued" {
		t.Errorf("promoted run status = %q, want queued", status)
	}
	// And the row was consumed, so it cannot fire twice.
	var left int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs WHERE id = ?`, id).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("pending rows left after promotion = %d, want 0", left)
	}
}

// TestPendingSweepByCreatorIsPossible — the recourse RB-Q12 depends on.
//
// Frozen authorization is only a defensible posture because an operator can find
// and cancel a departing user's parked runs. That requires the creator to be on
// the row and the row to be deletable; if either stopped being true, the decision
// would quietly become "revocation does nothing, and you cannot do anything about
// it" — which is not what was chosen.
func TestPendingSweepByCreatorIsPossible(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	seedPendingJob(t, pool)

	insertPending(t, pool, time.Now().Add(time.Hour)) // parked for later
	var byCreator int
	if err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_runs WHERE scheduled_by = 'op@example.com' AND status = 'pending'`).
		Scan(&byCreator); err != nil {
		t.Fatal(err)
	}
	if byCreator != 1 {
		t.Fatalf("parked runs found by creator = %d, want 1 — the offboarding runbook is a "+
			"by-creator query, so this column is the whole recourse", byCreator)
	}

	if _, err := pool.ExecContext(ctx,
		`DELETE FROM pending_runs WHERE scheduled_by = 'op@example.com'`); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Swept rows do not fire.
	scheduler.New(pool, discardLog(), nil).PromotePending(ctx)
	var runs int
	if err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE job_name = 'deferred-job'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("runs after sweeping the parked row = %d, want 0", runs)
	}
}
