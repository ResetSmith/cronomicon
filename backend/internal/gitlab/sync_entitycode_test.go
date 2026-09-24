// Why this file exists (LU-6 §5.6.1).
//
// This is the property the operator explicitly chose, and it is the one that is
// easiest to break by "tidying up": making the sync prune stamp deleted_at looks
// symmetrical and correct, and it is neither.
//
// A git job disappears from a sync for reasons NOBODY CHOSE — a repo
// reorganisation, a file rename, a transient YAML validation failure, a branch
// switch, a partial clone, a directory moved between refactors. It comes back on
// the next good sync. If the prune stamped the registry row deleted, the returning
// job would mint a FRESH code, its folder would be stranded on disk with no live
// entity pointing at it, and its run history would be split at every such cycle —
// permanently, since folders are never reclaimed (LU-Q5(a)).
//
// So: prune must NOT stamp, and the code must survive a full absent/return cycle
// unchanged. Only an explicit operator delete counts as a deletion (covered in
// internal/api/log_folder_test.go, which asserts the opposite outcome for the
// same tuple).
package gitlab

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/entitycode"
)

// liveCode returns the tuple's live registry code, failing if none exists.
func liveCode(t *testing.T, pool *sql.DB, kind, source, name string) string {
	t.Helper()
	// R2-5: the allocator keys on uid; resolve it the way sync does.
	table := "jobs"
	if kind == entitycode.KindWorkflow {
		table = "workflows"
	}
	var uid string
	if err := pool.QueryRow(`SELECT uid FROM `+table+` WHERE source=? AND name=?`, source, name).Scan(&uid); err != nil {
		// The definition may be pruned while its registry row lives on — which is
		// exactly what these tests assert. The registry row itself still knows
		// the identity it was allocated for.
		if err2 := pool.QueryRow(`SELECT COALESCE(uid,'') FROM entity_codes WHERE kind=? AND source=? AND name=? AND deleted_at IS NULL`,
			kind, source, name).Scan(&uid); err2 != nil {
			t.Fatalf("resolve uid %s/%s/%s: %v / %v", kind, source, name, err, err2)
		}
	}
	code, err := entitycode.Lookup(context.Background(), pool, kind, uid)
	if err != nil {
		t.Fatalf("lookup %s/%s/%s: %v", kind, source, name, err)
	}
	return code
}

// codeRowCount counts every registry row for a tuple, across eras.
func codeRowCount(t *testing.T, pool *sql.DB, kind, source, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM entity_codes WHERE kind=? AND source=? AND name=?`,
		kind, source, name).Scan(&n); err != nil {
		t.Fatalf("count codes: %v", err)
	}
	return n
}

// TestSyncAllocatesEntityCodesForImportedDefinitions is the precondition for
// everything below: the sync is the ONLY thing that ever allocates a code for a
// git-sourced definition, so if upsertJobs/upsertWorkflows stopped calling
// Allocate, every git job would fall back to the flat log layout with no error
// anywhere.
func TestSyncAllocatesEntityCodesForImportedDefinitions(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	gitCommitFile(t, repo, remote, "workflows/nightly.yaml",
		"apiVersion: amadeus.io/v1\nkind: Workflow\nmetadata:\n  name: nightly\nspec:\n  steps:\n    - name: s1\n      job: keep\n",
		"add workflow")

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync failed: %s", r.ErrorMessage)
	}
	jobCode := liveCode(t, svc.db, entitycode.KindJob, "git", "keep")
	if jobCode == "" {
		t.Fatal("sync imported job 'keep' but allocated no entity code — its runs would use the flat layout")
	}
	wfCode := liveCode(t, svc.db, entitycode.KindWorkflow, "git", "nightly")
	if wfCode == "" {
		t.Fatal("sync imported workflow 'nightly' but allocated no entity code")
	}
	if jobCode == wfCode {
		t.Errorf("job and workflow share code %q — they would share a log folder", jobCode)
	}

	// Re-syncing does not re-allocate: Allocate runs for every definition on every
	// sync, so a non-idempotent one would move a job's folder every few minutes.
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("second sync failed: %s", r.ErrorMessage)
	}
	if got := liveCode(t, svc.db, entitycode.KindJob, "git", "keep"); got != jobCode {
		t.Errorf("job code changed across syncs: %q → %q", jobCode, got)
	}
	if n := codeRowCount(t, svc.db, entitycode.KindJob, "git", "keep"); n != 1 {
		t.Errorf("registry holds %d rows for keep after two syncs, want 1", n)
	}
}

// TestSyncPruneAndReturnKeepsTheEntityCodeUnchanged is the §5.6.1 assertion. A
// job vanishes from the repo (pruned), then returns — and its code, its folder
// and therefore its whole run history must be exactly what they were. The
// registry row must also stay LIVE through the absence, because stamping it is
// what would mint a fresh code on return.
func TestSyncPruneAndReturnKeepsTheEntityCodeUnchanged(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	// 1. The job exists and holds a code.
	gitCommitFile(t, repo, remote, "jobs/temp.yaml", jobYAML("temp"), "add temp")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync 1 failed: %s", r.ErrorMessage)
	}
	original := liveCode(t, svc.db, entitycode.KindJob, "git", "temp")
	if original == "" {
		t.Fatal("no code allocated for the imported job")
	}
	backdate(t, svc.db)

	// 2. The job is gone from the repo, so the clean-parse prune deletes its row.
	gitRemoveFile(t, repo, "jobs/temp.yaml", "remove temp")
	if r := svc.SyncBlocking(ctx, "t"); r.Status != "success" {
		t.Fatalf("sync 2 = %q, want success; errs=%s", r.Status, r.ErrorMessage)
	}
	if jobCount(t, svc.db, "temp") != 0 {
		t.Fatal("temp was not pruned; the rest of this test would be vacuous")
	}
	// The registry row must be UNTOUCHED — still live, still the same code. A
	// prune is not a deletion: nobody chose it.
	if got := liveCode(t, svc.db, entitycode.KindJob, "git", "temp"); got != original {
		t.Fatalf("after prune the live code is %q, want the unchanged %q — the sync prune stamped deleted_at, which strands the job's log folder", got, original)
	}
	if n := codeRowCount(t, svc.db, entitycode.KindJob, "git", "temp"); n != 1 {
		t.Errorf("registry holds %d rows for temp after a prune, want 1 (no new era)", n)
	}

	// 3. The job returns — a repo reorganisation finished, the YAML parses again.
	gitCommitFile(t, repo, remote, "jobs/temp.yaml", jobYAML("temp"), "restore temp")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync 3 failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "temp") != 1 {
		t.Fatal("temp was not re-imported")
	}
	if got := liveCode(t, svc.db, entitycode.KindJob, "git", "temp"); got != original {
		t.Errorf("after the prune/return cycle the code is %q, want the original %q — the job's run history has been split across two folders", got, original)
	}
	if n := codeRowCount(t, svc.db, entitycode.KindJob, "git", "temp"); n != 1 {
		t.Errorf("registry holds %d rows for temp after a full cycle, want 1 — the cycle minted a new era", n)
	}
}

// TestSyncPruneDoesNotStampWorkflowEntityCodes covers the second prune site.
// upsertJobs and upsertWorkflows are separate code paths with separate prunes, so
// a fix (or a regression) applied to one does not carry to the other.
func TestSyncPruneDoesNotStampWorkflowEntityCodes(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	const wfYAML = "apiVersion: amadeus.io/v1\nkind: Workflow\nmetadata:\n  name: rollup\nspec:\n  steps:\n    - name: s1\n      job: keep\n"
	gitCommitFile(t, repo, remote, "workflows/rollup.yaml", wfYAML, "add rollup")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync 1 failed: %s", r.ErrorMessage)
	}
	original := liveCode(t, svc.db, entitycode.KindWorkflow, "git", "rollup")
	if original == "" {
		t.Fatal("no code allocated for the imported workflow")
	}
	if _, err := svc.db.Exec(`UPDATE workflows SET synced_at='2020-01-01T00:00:00Z' WHERE source='git'`); err != nil {
		t.Fatalf("backdate workflows: %v", err)
	}

	gitRemoveFile(t, repo, "workflows/rollup.yaml", "remove rollup")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync 2 failed: %s", r.ErrorMessage)
	}
	var stillThere int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM workflows WHERE name='rollup' AND source='git'`).Scan(&stillThere); err != nil {
		t.Fatalf("count workflow: %v", err)
	}
	if stillThere != 0 {
		t.Fatal("rollup was not pruned; the rest of this test would be vacuous")
	}
	if got := liveCode(t, svc.db, entitycode.KindWorkflow, "git", "rollup"); got != original {
		t.Errorf("after the workflow prune the live code is %q, want the unchanged %q", got, original)
	}

	gitCommitFile(t, repo, remote, "workflows/rollup.yaml", wfYAML, "restore rollup")
	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("sync 3 failed: %s", r.ErrorMessage)
	}
	if got := liveCode(t, svc.db, entitycode.KindWorkflow, "git", "rollup"); got != original {
		t.Errorf("after the workflow prune/return cycle the code is %q, want %q", got, original)
	}
}
