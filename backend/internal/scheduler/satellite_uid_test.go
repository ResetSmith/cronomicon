package scheduler_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// R2-2 — the pending_runs writers stamp the owner's uid.
//
// This matters more than it looks: the trigger's name-fallback arm means a
// writer that forgets the uid is INVISIBLE — deletes still cascade, nothing
// errors, and the omission only surfaces at R2-5 when the fallback becomes
// wrong. The fallback protects the data; this test protects the fallback from
// becoming load-bearing.
func TestPendingRunWritersStampOwnerUID(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	seedUIDJob(t, pool, "parked", "git", "uid-parked")

	id, err := scheduler.InsertPendingRun(ctx, pool, "job", "parked", "git", "", "2026-08-13T00:00:00Z", "alice", nil)
	if err != nil {
		t.Fatalf("InsertPendingRun: %v", err)
	}
	var uid sql.NullString
	if err := pool.QueryRowContext(ctx, `SELECT owner_uid FROM pending_runs WHERE id = ?`, id).Scan(&uid); err != nil {
		t.Fatalf("read owner_uid: %v", err)
	}
	if uid.String != "uid-parked" {
		t.Errorf("pending_runs.owner_uid = %v, want uid-parked", uid)
	}

	// And the cascade actually reaches it through the uid arm.
	if _, err := pool.ExecContext(ctx, `DELETE FROM jobs WHERE name='parked' AND source='git'`); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	var n int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d parked runs survived their job's deletion, want 0", n)
	}
}

// The reactor's own parking path is the second, near-identical INSERT; it is
// covered by TestReactionPendingRunStampsOwnerUID in gate_test.go, which lives
// in the scheduler package proper because pendingReaction is unexported.
