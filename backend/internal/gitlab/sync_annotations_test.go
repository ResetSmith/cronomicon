package gitlab

import (
	"context"
	"database/sql"
	"testing"
)

// AN-1 (the annotations plan) — annotations are operator-owned and
// sync-PRESERVED, the same contract tags hold under D6, reached by a different
// mechanism: tags are a column the upsert deliberately stops writing, while
// annotations live in a sidecar the upsert does not name at all.
//
// That difference is why these tests exist despite the table being
// structurally untouchable today. "Not in the query" is not a guarantee, it is
// a fact about the current query — and a future consolidation that folds the
// sidecar into the upsert (or a rebuild that re-derives it) would be a silent
// data loss with no failing test anywhere else in the tree.

// annotateOwner writes an annotation against a definition's CURRENT uid — the
// AN-2 PUT write, simulated directly.
//
// It resolves the uid rather than taking one, and fails when the lookup comes
// back empty: a uid-keyed sidecar seeded with ” would insert happily, match
// nothing, and let every assertion below pass for the wrong reason.
func annotateOwner(t *testing.T, pool *sql.DB, kind, source, name, notes string) {
	t.Helper()
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	var uid string
	if err := pool.QueryRow(
		`SELECT COALESCE(uid,'') FROM `+table+` WHERE source=? AND name=?`, source, name).Scan(&uid); err != nil {
		t.Fatalf("resolve %s uid for %s/%s: %v", kind, source, name, err)
	}
	if uid == "" {
		t.Fatalf("%s %s/%s has no uid — the sync upsert must stamp one, or this test proves nothing", kind, source, name)
	}
	if _, err := pool.Exec(`
		INSERT INTO annotations(owner_kind, owner_uid, critical, contact, notes, updated_by, updated_at)
		VALUES (?, ?, 1, 'dba-oncall@corp.example', ?, 'operator', '2026-08-14T00:00:00Z')`,
		kind, uid, notes); err != nil {
		t.Fatalf("insert annotation: %v", err)
	}
}

// annotationOf reads back by kind+uid only. There is deliberately no by-name
// read anywhere in this file: R2F-1's lesson is that the convenience of a name
// lookup is exactly how a sidecar starts serving the wrong twin.
func annotationOf(t *testing.T, pool *sql.DB, kind, source, name string) (notes, contact string, critical bool, found bool) {
	t.Helper()
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	var uid string
	if err := pool.QueryRow(
		`SELECT COALESCE(uid,'') FROM `+table+` WHERE source=? AND name=?`, source, name).Scan(&uid); err == sql.ErrNoRows {
		return "", "", false, false
	} else if err != nil {
		t.Fatalf("resolve uid: %v", err)
	}
	var crit int
	err := pool.QueryRow(`SELECT notes, contact, critical FROM annotations WHERE owner_kind=? AND owner_uid=?`,
		kind, uid).Scan(&notes, &contact, &crit)
	if err == sql.ErrNoRows {
		return "", "", false, false
	}
	if err != nil {
		t.Fatalf("read annotation: %v", err)
	}
	return notes, contact, crit == 1, true
}

func annotationRows(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM annotations`).Scan(&n); err != nil {
		t.Fatalf("count annotations: %v", err)
	}
	return n
}

// TestJobAnnotationSurvivesSync is the AN-1 twin of TestJobTagsSurviveSync: a
// re-sync that rewrites every git-owned column of a job must leave its
// annotation exactly as the operator left it. Drives the REAL upsertJobs.
func TestJobAnnotationSurvivesSync(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()
	// cloneDir lets resolveBodyHash run for an inline-less job (it reads nothing).
	svc := &Service{cloneDir: t.TempDir()}

	upsert := func(desc, now string) {
		t.Helper()
		j := JobYAML{}
		j.Metadata.Name = "backup"
		j.Spec.RunType = "bash"
		j.Spec.Scope = "Prod"
		j.Spec.Description = desc
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := svc.upsertJobs(ctx, tx, []JobYAML{j}, nil, nil, now, "sha"); err != nil {
			t.Fatalf("upsertJobs: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	upsert("v1", "t1")
	if _, _, _, found := annotationOf(t, pool, "job", "git", "backup"); found {
		t.Fatal("a freshly synced job already has an annotation — the sync path is writing this table")
	}

	annotateOwner(t, pool, "job", "git", "backup", "pages the DBA rota at 3am")

	// Second sync with a CHANGED description: the git-owned column moves,
	// proving the upsert actually ran, while the annotation stays put.
	upsert("v2", "t2")

	notes, contact, critical, found := annotationOf(t, pool, "job", "git", "backup")
	if !found {
		t.Fatal("annotation destroyed by re-sync — it must be sync-preserved (AN-1)")
	}
	if notes != "pages the DBA rota at 3am" {
		t.Errorf("notes = %q, want the operator's text", notes)
	}
	if contact != "dba-oncall@corp.example" || !critical {
		t.Errorf("contact=%q critical=%v — sync clobbered a field", contact, critical)
	}

	var gotDesc, gotSynced string
	if err := pool.QueryRow(`SELECT description, synced_at FROM jobs WHERE source='git' AND name='backup'`).
		Scan(&gotDesc, &gotSynced); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotDesc != "v2" || gotSynced != "t2" {
		t.Errorf("sync did not update Git-derived columns: description=%q synced=%q — the preservation above is vacuous", gotDesc, gotSynced)
	}
}

// TestWorkflowAnnotationSurvivesSync — the workflow half, written at the same
// time on purpose (the FX-D1 lesson: the last several defects in this area were
// each a guard applied to one twin and not the other).
func TestWorkflowAnnotationSurvivesSync(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()
	svc := &Service{} // upsertWorkflows uses only ctx/tx.

	upsert := func(desc, now string) {
		t.Helper()
		wf := WorkflowYAML{}
		wf.Metadata.Name = "deploy"
		wf.Spec.Description = desc
		wf.Spec.Steps = []any{}
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := svc.upsertWorkflows(ctx, tx, []WorkflowYAML{wf}, nil, now, "sha"); err != nil {
			t.Fatalf("upsertWorkflows: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	upsert("v1", "t1")
	if _, _, _, found := annotationOf(t, pool, "workflow", "git", "deploy"); found {
		t.Fatal("a freshly synced workflow already has an annotation — the sync path is writing this table")
	}

	annotateOwner(t, pool, "workflow", "git", "deploy", "release train: ask #platform before re-running")

	upsert("v2", "t2")

	notes, _, critical, found := annotationOf(t, pool, "workflow", "git", "deploy")
	if !found {
		t.Fatal("annotation destroyed by re-sync — it must be sync-preserved (AN-1)")
	}
	if notes != "release train: ask #platform before re-running" || !critical {
		t.Errorf("notes=%q critical=%v — sync clobbered the annotation", notes, critical)
	}

	var gotDesc, gotSynced string
	if err := pool.QueryRow(`SELECT COALESCE(description,''), synced_at FROM workflows WHERE source='git' AND name='deploy'`).
		Scan(&gotDesc, &gotSynced); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotDesc != "v2" || gotSynced != "t2" {
		t.Errorf("sync did not update Git-derived columns: description=%q synced=%q", gotDesc, gotSynced)
	}
}

// TestAnnotationPrunedWithGitJob is the lifecycle other half: when a job is
// REMOVED from Git and pruned, its annotation must go with it, or a re-added
// same-named job silently inherits notes about something else.
//
// The prune is a real DELETE, so migration 1060's trigger does the work and the
// PP-H9 orphan sweeps need no new arm. That is precisely why this test exists
// rather than a sweep: the R2F-1 STATUS note is a live warning that trigger
// arms get rewritten by later rebuilds, and a passing test is the only thing
// that notices.
func TestAnnotationPrunedWithGitJob(t *testing.T) {
	svc, repo, _ := newSyncFixture(t)
	ctx := context.Background()

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("initial sync failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "keep") != 1 {
		t.Fatalf("keep not imported")
	}

	annotateOwner(t, svc.db, "job", "git", "keep", "owned by finance; do not disable")
	if annotationRows(t, svc.db) != 1 {
		t.Fatalf("annotation not seeded")
	}

	backdate(t, svc.db)
	gitRemoveFile(t, repo, "jobs/keep.yaml", "remove keep")

	if r := svc.SyncBlocking(ctx, "t"); r.Status == "failed" {
		t.Fatalf("prune sync failed: %s", r.ErrorMessage)
	}
	if jobCount(t, svc.db, "keep") != 0 {
		t.Fatalf("keep was not pruned")
	}
	if n := annotationRows(t, svc.db); n != 0 {
		t.Errorf("annotation orphaned after prune: count=%d, want 0 — the 1060 cascade trigger is not firing", n)
	}
}
