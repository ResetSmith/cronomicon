package gitlab

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// A sync can add or prune a scope, and a scope's existence decides what an
// agency grant reaches. Since v2.3.0 grants are resolved from a cached snapshot
// (LR-78), so a sync that does not announce itself leaves a pruned scope in
// every session's reach until the snapshot's TTL.
//
// gitlab cannot be handed the auth Service, so the announcement is the
// process-wide counter; this asserts a sync advances it. (The announcement is a
// defer at the top of sync, so a sync that fails and rolls back announces too:
// it changed nothing, and saying so costs one rebuild.)
func TestEverySyncAnnouncesAGrantsChange(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	before := auth.GrantsGeneration()
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync failed: %s", r.ErrorMessage)
	}
	if auth.GrantsGeneration() == before {
		t.Fatal("a sync did not call auth.GrantsChanged")
	}

	// A sync that adds a scope.
	gitCommitFile(t, repo, remote, "inventory/prod.ini", "[web]\nweb1\n", "add a scope")
	before = auth.GrantsGeneration()
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync failed: %s", r.ErrorMessage)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM scopes WHERE name='prod' AND source='git'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the scope was not synced (n=%d, err=%v)", n, err)
	}
	if auth.GrantsGeneration() == before {
		t.Error("a sync that added a scope did not call auth.GrantsChanged")
	}
}
