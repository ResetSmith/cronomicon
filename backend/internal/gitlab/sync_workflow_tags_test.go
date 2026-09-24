package gitlab

import (
	"context"
	"testing"
)

// TestWorkflowTagsSurviveSync mirrors TestScriptTagsSurviveSync for workflows
// (migration 290): operator-authored tags written via PUT /workflow-tags must NOT
// be clobbered when a later sync re-upserts the same workflow with changed content.
// Drives the REAL upsertWorkflows.
func TestWorkflowTagsSurviveSync(t *testing.T) {
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

	// First sync: inserted; tags take the column DEFAULT '[]'.
	upsert("v1", "t1")

	var tags string
	if err := pool.QueryRow(`SELECT tags FROM workflows WHERE source='git' AND name='deploy'`).Scan(&tags); err != nil {
		t.Fatalf("read tags after first sync: %v", err)
	}
	if tags != "[]" {
		t.Fatalf("after first sync tags = %q, want %q (DEFAULT)", tags, "[]")
	}

	// Operator tags the workflow (the PUT /workflow-tags write, simulated directly).
	if _, err := pool.ExecContext(ctx, `UPDATE workflows SET tags=? WHERE source='git' AND name='deploy'`, `["release"]`); err != nil {
		t.Fatalf("tag update: %v", err)
	}

	// Second sync: same workflow, CHANGED description — proves the upsert ran
	// (description + synced_at updated) while leaving tags untouched.
	upsert("v2", "t2")

	var gotTags, gotDesc, gotSynced string
	if err := pool.QueryRow(`SELECT tags, COALESCE(description,''), synced_at FROM workflows WHERE source='git' AND name='deploy'`).
		Scan(&gotTags, &gotDesc, &gotSynced); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTags != `["release"]` {
		t.Errorf("tags clobbered by sync: got %q, want %q", gotTags, `["release"]`)
	}
	if gotDesc != "v2" || gotSynced != "t2" {
		t.Errorf("sync did not update Git-derived columns: description=%q synced=%q", gotDesc, gotSynced)
	}
}
