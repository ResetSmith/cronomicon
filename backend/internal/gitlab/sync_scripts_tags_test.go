package gitlab

import (
	"context"
	"testing"
)

// TestScriptTagsSurviveSync is the load-bearing guarantee of scripts-tags.md: a
// script's user-authored tags (written outside sync, via PUT /script-tags) must
// NOT be clobbered when a later sync re-upserts the same script with changed
// content. It drives the REAL upsertScripts, so a regression that "completes the
// pattern" by adding tags=excluded.tags to the ON CONFLICT DO UPDATE is caught
// here rather than in production.
func TestScriptTagsSurviveSync(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()
	svc := &Service{} // upsertScripts uses only ctx/tx, so a zero Service is enough.

	upsert := func(rs resolvedScript, now string) {
		t.Helper()
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := svc.upsertScripts(ctx, tx, map[string]resolvedScript{"backup": rs}, now, "sha"); err != nil {
			t.Fatalf("upsertScripts: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// First sync: the script is inserted; tags take the column DEFAULT '[]'.
	upsert(resolvedScript{runType: "bash", command: "echo hi", contentHash: "sha256:aaa", sourcePath: "scripts/backup.yaml", warnings: "[]", variables: "[]"}, "t1")

	var tags string
	if err := pool.QueryRow(`SELECT tags FROM scripts WHERE name='backup'`).Scan(&tags); err != nil {
		t.Fatalf("read tags after first sync: %v", err)
	}
	if tags != "[]" {
		t.Fatalf("after first sync tags = %q, want %q (DEFAULT)", tags, "[]")
	}

	// Operator tags the script (the PUT /script-tags write, simulated directly).
	if _, err := pool.ExecContext(ctx, `UPDATE scripts SET tags=? WHERE name='backup'`, `["prod","db"]`); err != nil {
		t.Fatalf("tag update: %v", err)
	}

	// Second sync: same script, CHANGED body — proves the upsert actually ran
	// (content_hash + command + synced_at updated) while leaving tags untouched.
	upsert(resolvedScript{runType: "bash", command: "echo bye", contentHash: "sha256:bbb", sourcePath: "scripts/backup.yaml", warnings: "[]", variables: "[]"}, "t2")

	var gotTags, gotCmd, gotHash, gotSynced string
	if err := pool.QueryRow(`SELECT tags, command, content_hash, synced_at FROM scripts WHERE name='backup'`).
		Scan(&gotTags, &gotCmd, &gotHash, &gotSynced); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTags != `["prod","db"]` {
		t.Errorf("tags clobbered by sync: got %q, want %q", gotTags, `["prod","db"]`)
	}
	if gotCmd != "echo bye" || gotHash != "sha256:bbb" || gotSynced != "t2" {
		t.Errorf("sync did not update Git-derived columns: command=%q hash=%q synced=%q", gotCmd, gotHash, gotSynced)
	}
}
