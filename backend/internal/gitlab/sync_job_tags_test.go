package gitlab

import (
	"context"
	"testing"
)

// TestJobTagsSurviveSync is the D6 regression guard (tags-support.md): jobs.tags is
// now operator-owned, so a re-sync — even one whose YAML spec.tags CHANGED — must
// NOT overwrite the DB tags, and the first sync must NOT seed tags from spec.tags.
// Drives the REAL upsertJobs. A regression that restores tags=excluded.tags (or
// re-adds the json.Marshal(j.Spec.Tags) bound value) is caught here.
func TestJobTagsSurviveSync(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()
	// cloneDir lets resolveBodyHash run for an inline-less job (it reads nothing).
	svc := &Service{cloneDir: t.TempDir()}

	upsert := func(yamlTags []string, desc, now string) {
		t.Helper()
		j := JobYAML{}
		j.Metadata.Name = "backup"
		j.Spec.RunType = "bash"
		j.Spec.Scope = "Prod"
		j.Spec.Description = desc
		j.Spec.Tags = yamlTags // parsed-but-unused under D6
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

	// First sync: even though spec.tags=["fromyaml"], D6 means it is NOT applied —
	// the column takes its DEFAULT '[]'. (Pre-D6 this assertion would fail.)
	upsert([]string{"fromyaml"}, "v1", "t1")

	var tags string
	if err := pool.QueryRow(`SELECT tags FROM jobs WHERE source='git' AND name='backup'`).Scan(&tags); err != nil {
		t.Fatalf("read tags after first sync: %v", err)
	}
	if tags != "[]" {
		t.Fatalf("D6 violated: YAML spec.tags landed in jobs.tags on first sync (got %q, want [])", tags)
	}

	// Operator tags the job (the PUT /job-tags write, simulated directly).
	if _, err := pool.ExecContext(ctx, `UPDATE jobs SET tags=? WHERE source='git' AND name='backup'`, `["prod","db"]`); err != nil {
		t.Fatalf("tag update: %v", err)
	}

	// Second sync with DIFFERENT spec.tags AND a changed description: the
	// description updates, the operator tags survive, the new YAML tags are ignored.
	upsert([]string{"different", "yaml"}, "v2", "t2")

	var gotTags, gotDesc, gotSynced string
	if err := pool.QueryRow(`SELECT tags, description, synced_at FROM jobs WHERE source='git' AND name='backup'`).
		Scan(&gotTags, &gotDesc, &gotSynced); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTags != `["prod","db"]` {
		t.Errorf("D6 violated: operator tags clobbered by sync (got %q, want %q)", gotTags, `["prod","db"]`)
	}
	if gotDesc != "v2" || gotSynced != "t2" {
		t.Errorf("sync did not update Git-derived columns: description=%q synced=%q", gotDesc, gotSynced)
	}
}
