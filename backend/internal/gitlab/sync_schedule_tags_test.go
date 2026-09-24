package gitlab

import (
	"context"
	"testing"
)

// TestScheduleTagsSurviveSync mirrors TestScriptTagsSurviveSync for schedules
// (migration 290): operator-authored tags written via PUT /schedule-tags must NOT
// be clobbered when a later sync re-upserts the same schedule with changed content.
// Drives the REAL upsertSchedules so a regression that "completes the pattern" by
// adding tags=excluded.tags to the ON CONFLICT DO UPDATE is caught here.
func TestScheduleTagsSurviveSync(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()
	svc := &Service{} // upsertSchedules uses only ctx/tx.

	upsert := func(rs resolvedSchedule, now string) {
		t.Helper()
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := svc.upsertSchedules(ctx, tx, map[string]resolvedSchedule{"nightly": rs}, now, "sha"); err != nil {
			t.Fatalf("upsertSchedules: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// First sync: inserted; tags take the column DEFAULT '[]'.
	upsert(resolvedSchedule{cron: "0 0 * * *", contentHash: "sha256:aaa", sourcePath: "schedules/nightly.yaml"}, "t1")

	var tags string
	if err := pool.QueryRow(`SELECT tags FROM schedules WHERE source='git' AND name='nightly'`).Scan(&tags); err != nil {
		t.Fatalf("read tags after first sync: %v", err)
	}
	if tags != "[]" {
		t.Fatalf("after first sync tags = %q, want %q (DEFAULT)", tags, "[]")
	}

	// Operator tags the schedule (the PUT /schedule-tags write, simulated directly).
	if _, err := pool.ExecContext(ctx, `UPDATE schedules SET tags=? WHERE source='git' AND name='nightly'`, `["prod","cron"]`); err != nil {
		t.Fatalf("tag update: %v", err)
	}

	// Second sync: same schedule, CHANGED cron — proves the upsert actually ran
	// (cron + content_hash + synced_at updated) while leaving tags untouched.
	upsert(resolvedSchedule{cron: "*/5 * * * *", contentHash: "sha256:bbb", sourcePath: "schedules/nightly.yaml"}, "t2")

	var gotTags, gotCron, gotHash, gotSynced string
	if err := pool.QueryRow(`SELECT tags, cron, content_hash, synced_at FROM schedules WHERE source='git' AND name='nightly'`).
		Scan(&gotTags, &gotCron, &gotHash, &gotSynced); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTags != `["prod","cron"]` {
		t.Errorf("tags clobbered by sync: got %q, want %q", gotTags, `["prod","cron"]`)
	}
	if gotCron != "*/5 * * * *" || gotHash != "sha256:bbb" || gotSynced != "t2" {
		t.Errorf("sync did not update Git-derived columns: cron=%q hash=%q synced=%q", gotCron, gotHash, gotSynced)
	}
}
