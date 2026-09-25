package gitlab

import (
	"context"
	"database/sql"
	"testing"
)

func insertPause(t *testing.T, pool *sql.DB, source, kind, name string) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at, owner_uid)
		VALUES (?, ?, ?, 'test', '2020-01-01T00:00:00Z',
			CASE ?
			  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
			  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
			END)`, source, kind, name, kind, name, source, name, source); err != nil {
		t.Fatalf("insert pause %s/%s/%s: %v", source, kind, name, err)
	}
}

func pausedCount(t *testing.T, pool *sql.DB, source, kind, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`
		SELECT COUNT(*) FROM paused_jobs WHERE source=? AND owner_kind=? AND name=?`,
		source, kind, name).Scan(&n); err != nil {
		t.Fatalf("count pause: %v", err)
	}
	return n
}

// TestSyncPruneCleansPausedJobRow is the PP-H9 core regression: when a paused
// git job is removed from Git and pruned, its paused_jobs row must NOT survive
// (else a re-added same-named job silently inherits the stale pause).
func TestSyncPruneCleansPausedJobRow(t *testing.T) {
	svc, repo, _ := newSyncFixture(t)
	ctx := context.Background()

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("initial sync failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("keep not imported")
	}
	// Pause the git job, then remove it from Git.
	insertPause(t, svc.db, "git", "job", "keep")
	backdate(t, svc.db)
	gitRemoveFile(t, repo, "jobs/keep.yaml", "remove keep")

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("prune sync failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "keep") != 0 {
		t.Fatalf("keep was not pruned")
	}
	if c := pausedCount(t, svc.db, "git", "job", "keep"); c != 0 {
		t.Errorf("paused_jobs row orphaned after prune: count=%d, want 0 (PP-H9)", c)
	}
}

// TestSyncPruneSkippedKeepsPausedJobRow: when the jobs subsystem parse fails
// (PP-B2 skips its prune), the still-valid job is retained AND its pause must be
// retained too — the orphan sweep is gated inside jobsOK, so a parse blip can't
// drop a legitimate pause.
func TestSyncPruneSkippedKeepsPausedJobRow(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("initial sync failed: %s", r.ErrorMessage)
	}
	insertPause(t, svc.db, "git", "job", "keep")
	backdate(t, svc.db)

	// Corrupt keep.yaml → parseJobs validation error → jobsOK=false.
	gitCommitFile(t, repo, remote, "jobs/keep.yaml",
		"apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: keep\n", "corrupt keep")

	r := svc.SyncBlocking(ctx, "t")
	if r.Status != "partial" {
		t.Errorf("status = %q, want partial", r.Status)
	}
	if jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("keep wrongly pruned on parse error (PP-B2)")
	}
	if c := pausedCount(t, svc.db, "git", "job", "keep"); c != 1 {
		t.Errorf("pause wrongly swept while job retained: count=%d, want 1 (OK-gate)", c)
	}
}

// TestPausedCascadeTriggerOnBareDelete isolates migration 200's triggers: a bare
// DELETE FROM jobs / workflows (no app-layer or prune-layer cleanup) must remove
// the matching paused_jobs row via the AFTER DELETE trigger alone, for both
// owner kinds.
func TestPausedCascadeTriggerOnBareDelete(t *testing.T) {
	svc, _, _ := newSyncFixture(t) // we need only its migrated DB
	pool := svc.db

	if _, err := pool.Exec(`INSERT INTO jobs(uid,name,source,run_type,command,synced_at) VALUES('uid-aj','aj','amadeus','bash','echo',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO workflows(uid,name,source,steps,synced_at) VALUES('uid-awf','awf','amadeus','[]',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	insertPause(t, pool, "amadeus", "job", "aj")
	insertPause(t, pool, "amadeus", "workflow", "awf")

	if _, err := pool.Exec(`DELETE FROM jobs WHERE source='amadeus' AND name='aj'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`DELETE FROM workflows WHERE source='amadeus' AND name='awf'`); err != nil {
		t.Fatal(err)
	}
	if c := pausedCount(t, pool, "amadeus", "job", "aj"); c != 0 {
		t.Errorf("job pause not cascaded by trigger: count=%d, want 0", c)
	}
	if c := pausedCount(t, pool, "amadeus", "workflow", "awf"); c != 0 {
		t.Errorf("workflow pause not cascaded by trigger: count=%d, want 0", c)
	}
}
