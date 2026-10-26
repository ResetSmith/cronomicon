package scheduler

import (
	"context"
	"testing"
	"time"
)

// The record of a JOB's fire that did not happen is of one definition, as a
// workflow's is (2.4.0 Phase R6; found by the review of the phase).
//
// 1c84d50 made the pause, the calendar and the watch for a missed fire of one
// definition, and the record of a skipped fire of a WORKFLOW. A job's was left
// by name: the rule that decides whether a skip is recorded already matched
// job_name, job_source and the entry, and the record of a missed fire carried
// no identity at all (so the row took whichever job of the name the database
// returned first, and that one's run type). Two agencies' jobs of one name
// that both missed a fire, were both paused, or both sat on a closed calendar
// left ONE row between them.
func TestTwoJobsOfOneNameEachLeaveTheirOwnRecordOfAFireThatDidNotHappen(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	rows := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seedFinTax(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, owner_agency) VALUES
	        ('j-fin', 'load', 'cronomicon', 'bash', 'Allow', 1, 't', 'ag-fin'), ('j-tax', 'load', 'cronomicon', 'python', 'Allow', 1, 't', 'ag-tax')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, skip_calendars) VALUES
	        ('cronomicon', 'job', 'load', 'at-two', '0 2 * * *', 0, 'j-fin', '["closed"]'),
	        ('cronomicon', 'job', 'load', 'at-two', '0 2 * * *', 0, 'j-tax', '["closed"]')`)
	s := New(pool, quietLog(), nil)
	runType := map[string]string{"j-fin": "bash", "j-tax": "python"}
	skipped := func(uid, and string) int {
		return rows(`SELECT COUNT(*) FROM runs WHERE job_uid = ? AND status = 'skipped' `+and, uid)
	}
	fire := func(uid string) { s.fire("cronomicon", "load", uid, runType[uid], "", "Allow", "", "at-two", "") }

	// ── A missed fire. ──────────────────────────────────────────────────────
	entries, err := s.scheduleEntriesForDetection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the entries watched: %+v, want the two jobs' one each", entries)
	}
	at := time.Now().UTC().Truncate(time.Second)
	for i, e := range entries {
		if got, err := s.fireExplained(ctx, e, at, s.location()); err != nil || got {
			t.Fatalf("%s's fire, which nothing has recorded, reads as explained (%v): by the other job's record, if this is the second (%d)", e.ownerUID, err, i)
		}
		s.recordMissedFire(ctx, e, at)
		s.recordMissedFire(ctx, e, at) // the same episode again: one row stands for it
	}
	for uid, rt := range runType {
		if n := skipped(uid, `AND queued_reason = '`+reasonMissedFire+`'`); n != 1 {
			t.Errorf("%s has %d records of its missed fire, want 1 of its own", uid, n)
		}
		if n := skipped(uid, `AND run_type = '`+rt+`'`); n != 1 {
			t.Errorf("%s's record of its missed fire is not of its own run type (%s)", uid, rt)
		}
	}
	if n := rows(`SELECT COUNT(*) FROM runs`); n != 2 {
		t.Errorf("%d rows were written for two missed fires, want 2", n)
	}
	for _, e := range entries {
		if got, err := s.fireExplained(ctx, e, at, s.location()); err != nil || !got {
			t.Errorf("%s's missed fire, which is recorded, reads as not explained (%v)", e.ownerUID, err)
		}
	}
	exec(`DELETE FROM runs`)

	// ── A pause (one record for the episode, per definition). ───────────────
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid) VALUES
	        ('cronomicon', 'job', 'load', 'u', 't', 'j-fin'), ('cronomicon', 'job', 'load', 'u', 't', 'j-tax')`)
	for range 2 {
		fire("j-fin")
		fire("j-tax")
	}
	for uid := range runType {
		if n := skipped(uid, `AND queued_reason = '`+reasonJobPaused+`'`); n != 1 {
			t.Errorf("%s, paused and fired twice, has %d records of it, want 1 of its own", uid, n)
		}
	}
	exec(`DELETE FROM paused_jobs`)
	exec(`DELETE FROM runs`)

	// ── A closed calendar (one record for the day, per definition). ─────────
	// Each agency's own calendar of the name (a definition's calendars are its
	// agency's, §0a).
	seedCalOf(t, pool, "ag-fin", "closed", false, true, map[string]string{today(s): "closed"})
	seedCalOf(t, pool, "ag-tax", "closed", false, true, map[string]string{today(s): "closed"})
	for range 2 {
		fire("j-fin")
		fire("j-tax")
	}
	for uid := range runType {
		if n := skipped(uid, `AND suppressed_by_calendar = 'closed'`); n != 1 {
			t.Errorf("%s, fired twice on a closed day, has %d records of it, want 1 of its own", uid, n)
		}
	}
	if n := rows(`SELECT COUNT(*) FROM runs`); n != 2 {
		t.Errorf("%d rows were written for the closed day, want one for each job", n)
	}

	// ── A row that records no identity is found by its name, as before. ─────
	exec(`DELETE FROM runs`)
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, schedule_name, status, queued_reason, suppressed_by_calendar, triggered_by, trigger_kind, created_at)
	        VALUES ('r-old', 'load', 'cronomicon', 'bash', 'at-two', 'skipped', 'old', 'closed', 'scheduler', 'scheduled', ?)`, time.Now().UTC().Format(time.RFC3339))
	fire("j-fin")
	if n := rows(`SELECT COUNT(*) FROM runs`); n != 1 {
		t.Errorf("a record of today's closed day that carries no identity did not stand for the job of its name: %d rows", n)
	}
}
