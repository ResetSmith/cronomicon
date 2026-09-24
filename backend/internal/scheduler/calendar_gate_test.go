package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// CAL-21 — the enforcement half. These tests are the audit trail's regression
// fence: CAL-8's de-dupe is the one bug that would leave the feature apparently
// working while quietly gutting the evidence it exists to produce.

// today renders the current instant the way the gate does — in the scheduler's
// live zone, never UTC.
func today(s *Scheduler) string { return calendar.DayOf(time.Now(), s.location()) }

func seedCal(t *testing.T, pool *sql.DB, name string, global, record bool, days map[string]string) {
	t.Helper()
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO calendars (source, name, global, record_suppressed, created_at)
		 VALUES ('amadeus', ?, ?, ?, '2026-08-06T00:00:00Z')`, name, b(global), b(record)); err != nil {
		t.Fatalf("seed calendar %s: %v", name, err)
	}
	for day, label := range days {
		if _, err := pool.ExecContext(context.Background(),
			`INSERT INTO calendar_days (calendar_source, calendar_name, day, label)
			 VALUES ('amadeus', ?, ?, ?)`, name, day, label); err != nil {
			t.Fatalf("seed day %s/%s: %v", name, day, err)
		}
	}
}

// bindSchedule writes the runtime schedule row the gate reads fresh per fire.
func bindSchedule(t *testing.T, pool *sql.DB, kind, owner, name, skipJSON, onlyJSON string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO definition_schedules
			(owner_source, owner_kind, owner_name, name, cron, position, skip_calendars, only_calendars)
		 VALUES ('git', ?, ?, ?, '0 2 * * *', 0, ?, ?)`,
		kind, owner, name, nullStr(skipJSON), nullStr(onlyJSON)); err != nil {
		t.Fatalf("bind schedule %s/%s: %v", owner, name, err)
	}
}

func countRuns(t *testing.T, pool *sql.DB, job, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runs WHERE job_name = ? AND status = ?`, job, status).Scan(&n); err != nil {
		t.Fatalf("count %s runs: %v", status, err)
	}
	return n
}

// A fire landing on a skip-calendar day is suppressed and recorded, with the
// calendar named in the structured provenance column — not merely in the text.
func TestFireSuppressedBySkipCalendarRecords(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	seedCal(t, pool, "federal-holidays", false, false, map[string]string{today(s): "Independence Day (observed)"})
	bindSchedule(t, pool, "job", "patch", "nightly", `["federal-holidays"]`, "")

	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "patch", "queued"); n != 0 {
		t.Errorf("suppressed fire enqueued %d runs, want 0", n)
	}
	var reason, byCal, schedName sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT queued_reason, suppressed_by_calendar, schedule_name FROM runs WHERE job_name='patch' AND status='skipped'`).
		Scan(&reason, &byCal, &schedName); err != nil {
		t.Fatalf("fetch skipped run: %v", err)
	}
	if byCal.String != "federal-holidays" {
		t.Errorf("suppressed_by_calendar = %q, want federal-holidays — the History filter queries this column, not the reason text", byCal.String)
	}
	if schedName.String != "nightly" {
		t.Errorf("schedule_name = %q, want nightly", schedName.String)
	}
	if reason.String == "" || !strings.Contains(reason.String, "Independence Day (observed)") {
		t.Errorf("queued_reason = %q, want the day's label in it", reason.String)
	}
}

// An ordinary day still fires — the gate must not be a blanket suppressor.
func TestFireNotSuppressedOnOrdinaryDay(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	seedCal(t, pool, "federal-holidays", false, false, map[string]string{"1999-01-01": "long ago"})
	bindSchedule(t, pool, "job", "patch", "nightly", `["federal-holidays"]`, "")

	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "patch", "queued"); n != 1 {
		t.Errorf("enqueued %d runs, want 1 — an unmatched calendar must not suppress", n)
	}
	if n := countRuns(t, pool, "patch", "skipped"); n != 0 {
		t.Errorf("recorded %d skips on an ordinary day, want 0", n)
	}
}

// only-polarity: suppressed off its run days, and silent about it unless the
// calendar opted in (CAL-Q4). Both settings asserted.
func TestFireOnlyPolarityRecordingIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		record     bool
		wantSkips  int
		wantQueued int
	}{
		{"default is silent", false, 0, 0},
		{"opted in records", true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)

			seedJobRow(t, pool, "sweep", 1)
			// A run-day set that does NOT contain today.
			seedCal(t, pool, "fiscal-close", false, tc.record, map[string]string{"1999-09-30": "fiscal close"})
			bindSchedule(t, pool, "job", "sweep", "nightly", "", `["fiscal-close"]`)

			s.fire("git", "sweep", "", "bash", "prod", "Allow", "", "nightly", "")

			if n := countRuns(t, pool, "sweep", "queued"); n != tc.wantQueued {
				t.Errorf("enqueued %d, want %d — an off-day only-fire must be suppressed", n, tc.wantQueued)
			}
			if n := countRuns(t, pool, "sweep", "skipped"); n != tc.wantSkips {
				t.Errorf("recorded %d skips, want %d", n, tc.wantSkips)
			}
		})
	}
}

// A day inside the only-set fires normally.
func TestFireOnlyPolarityRunsOnRunDay(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "sweep", 1)
	seedCal(t, pool, "fiscal-close", false, false, map[string]string{today(s): "close"})
	bindSchedule(t, pool, "job", "sweep", "nightly", "", `["fiscal-close"]`)

	s.fire("git", "sweep", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "sweep", "queued"); n != 1 {
		t.Errorf("enqueued %d runs on a run day, want 1", n)
	}
}

// §2.8 — a global calendar suppresses an entry that names no calendars at all.
// This is the change-freeze case, and the reason must NAME the global calendar
// or nobody can tell why a job with no bindings stopped firing.
func TestFireSuppressedByGlobalCalendar(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "anything", 1)
	seedCal(t, pool, "change-freeze-2026q3", true, false, map[string]string{today(s): "Q3 freeze"})
	bindSchedule(t, pool, "job", "anything", "nightly", "", "") // no bindings of its own

	s.fire("git", "anything", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "anything", "queued"); n != 0 {
		t.Errorf("enqueued %d runs during a global freeze, want 0", n)
	}
	var byCal sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT suppressed_by_calendar FROM runs WHERE job_name='anything' AND status='skipped'`).Scan(&byCal); err != nil {
		t.Fatalf("fetch skipped run: %v", err)
	}
	if byCal.String != "change-freeze-2026q3" {
		t.Errorf("suppressed_by_calendar = %q, want the global calendar named", byCal.String)
	}
}

// CAL-8 / PP-L8 — an Allow-policy job carries a NULL concurrency_key, so a
// key-based de-dupe would never collapse anything: a minute-by-minute entry
// would write ~1,440 audit rows for ONE suppressed day.
func TestCalendarSkipDeDupesPerDayNotPerConcurrencyKey(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "minutely", 1)
	seedCal(t, pool, "holidays", false, false, map[string]string{today(s): "holiday"})
	bindSchedule(t, pool, "job", "minutely", "everyminute", `["holidays"]`, "")

	for range 5 {
		s.fire("git", "minutely", "", "bash", "prod", "Allow", "", "everyminute", "")
	}

	if n := countRuns(t, pool, "minutely", "skipped"); n != 1 {
		t.Fatalf("recorded %d skips for one suppressed day, want 1", n)
	}
	var key sql.NullString
	_ = pool.QueryRowContext(context.Background(),
		`SELECT concurrency_key FROM runs WHERE job_name='minutely' AND status='skipped'`).Scan(&key)
	if key.Valid {
		t.Errorf("concurrency_key = %q, want NULL — PP-L8 assigns it only to Forbid runs", key.String)
	}
}

// The day de-dupe must not collapse ACROSS days: a three-day holiday weekend is
// three suppressed days and must leave three records.
func TestCalendarSkipRecordsOncePerDayAcrossDays(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "daily", 1)
	seedCal(t, pool, "holidays", false, false, map[string]string{today(s): "holiday"})
	bindSchedule(t, pool, "job", "daily", "nightly", `["holidays"]`, "")

	// A suppression already recorded YESTERDAY must not swallow today's.
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO runs (id, job_name, job_source, run_type, status, queued_reason, triggered_by,
			trigger_kind, schedule_name, suppressed_by_calendar, created_at)
		 VALUES ('yesterday', 'daily', 'git', 'bash', 'skipped', 'r', 'scheduler', 'scheduled', 'nightly', 'holidays', ?)`,
		time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed yesterday's skip: %v", err)
	}

	s.fire("git", "daily", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "daily", "skipped"); n != 2 {
		t.Errorf("recorded %d skips, want 2 (yesterday's + today's) — the day de-dupe must not collapse across days", n)
	}
}

// CAL-8's headline: the two de-dupe modes are INDEPENDENT. A Forbid skip landing
// between two calendar skips swallows neither, and is not itself swallowed.
func TestSkipDeDupeModesAreIndependent(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	loc := time.UTC
	day := calendar.DayOf(time.Now(), loc)
	prev := calendar.DayOf(time.Now().Add(-24*time.Hour), loc)

	p := EnqueueParams{JobName: "j", JobSource: "git", RunType: "bash", ScheduleName: "nightly", ConcurrencyKey: "j"}

	// A calendar skip recorded for yesterday.
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{
		Reason: "cal yesterday", Mode: dedupeDay, Calendar: "holidays", Day: prev, Loc: loc,
	}); err != nil {
		t.Fatalf("calendar skip #1: %v", err)
	}
	// Backdate it so "today" is genuinely a different day from its created_at.
	if _, err := pool.ExecContext(ctx,
		`UPDATE runs SET created_at = ? WHERE suppressed_by_calendar = 'holidays'`,
		time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// A Forbid skip in between. It must NOT be swallowed by the calendar row that
	// happens to be the most recent run for this key.
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{
		Reason: "forbid", Mode: dedupeEpisode,
	}); err != nil {
		t.Fatalf("forbid skip: %v", err)
	}

	// Today's calendar skip must NOT be swallowed by the Forbid row either.
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{
		Reason: "cal today", Mode: dedupeDay, Calendar: "holidays", Day: day, Loc: loc,
	}); err != nil {
		t.Fatalf("calendar skip #2: %v", err)
	}

	var total, calendarRows int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_name='j' AND status='skipped'`).Scan(&total)
	_ = pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE job_name='j' AND status='skipped' AND suppressed_by_calendar IS NOT NULL`).Scan(&calendarRows)
	if total != 3 {
		t.Errorf("recorded %d skips, want 3 (calendar, forbid, calendar) — the modes must not swallow each other", total)
	}
	if calendarRows != 2 {
		t.Errorf("recorded %d calendar skips, want 2", calendarRows)
	}

	// And the episode rule still holds within its own kind: a second Forbid skip
	// while the last non-calendar run is already skipped collapses.
	if _, err := recordSkippedFire(ctx, pool, p, skipRecord{Reason: "forbid again", Mode: dedupeEpisode}); err != nil {
		t.Fatalf("forbid skip #2: %v", err)
	}
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_name='j' AND status='skipped'`).Scan(&total)
	if total != 3 {
		t.Errorf("after a repeat Forbid skip: %d rows, want 3 (episode de-dupe still collapses its own kind)", total)
	}
}

// CAL-32 — the workflow half of the audit trail. Before this, a suppressed
// workflow fire left nothing behind at all.
func TestFireWorkflowSuppressedByCalendarRecords(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)

	var fired []string
	s.SetWorkflowFirer(func(_ context.Context, _ string, name, _, _ string) { fired = append(fired, name) })

	if _, err := pool.ExecContext(ctx, `INSERT INTO workflows (name, enabled, synced_at) VALUES ('release', 1, 't')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	seedCal(t, pool, "federal-holidays", false, false, map[string]string{today(s): "Veterans Day"})
	bindSchedule(t, pool, "workflow", "release", "nightly", `["federal-holidays"]`, "")

	s.fireWorkflow("git", "release", "nightly", "")

	if len(fired) != 0 {
		t.Errorf("workflow fired %v during a calendar suppression, want none", fired)
	}
	var status, reason, byCal sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT status, queued_reason, suppressed_by_calendar FROM workflow_runs WHERE workflow_name='release'`).
		Scan(&status, &reason, &byCal); err != nil {
		t.Fatalf("fetch skipped workflow run: %v", err)
	}
	if status.String != "skipped" {
		t.Errorf("status = %q, want skipped", status.String)
	}
	if byCal.String != "federal-holidays" {
		t.Errorf("suppressed_by_calendar = %q, want federal-holidays", byCal.String)
	}
	if !strings.Contains(reason.String, "Veterans Day") {
		t.Errorf("queued_reason = %q, want the day's label in it", reason.String)
	}

	// Same per-day de-dupe as the job path.
	s.fireWorkflow("git", "release", "nightly", "")
	var n int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs WHERE workflow_name='release'`).Scan(&n)
	if n != 1 {
		t.Errorf("recorded %d workflow skips for one day, want 1", n)
	}
}

// §2.6 — a calendar gates SCHEDULED fires only. A human clicking Run on a
// holiday is a human deciding to run on a holiday. Tested explicitly because it
// is a deliberate asymmetry that someone will later mistake for a bug.
func TestManualEnqueueIsNeverSuppressedByCalendar(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	seedCal(t, pool, "federal-holidays", false, false, map[string]string{today(s): "holiday"})
	bindSchedule(t, pool, "job", "patch", "nightly", `["federal-holidays"]`, "")

	if err := EnqueueRun(ctx, pool, EnqueueParams{
		JobName: "patch", JobSource: "git", RunType: "bash", Scope: "prod",
		TriggerKind: "manual", TriggeredBy: "operator@example.com",
	}); err != nil {
		t.Fatalf("manual enqueue: %v", err)
	}
	if n := countRuns(t, pool, "patch", "queued"); n != 1 {
		t.Errorf("manual run enqueued %d, want 1 — calendars gate scheduled fires only", n)
	}
}

// A dangling skip binding fires (fail open); a dangling only binding does not
// (fail closed). The §2.2 backstop, exercised through the real gate.
func TestDanglingBindingsFailTowardAuthoredIntent(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "skipper", 1)
	bindSchedule(t, pool, "job", "skipper", "nightly", `["nonexistent"]`, "")
	s.fire("git", "skipper", "", "bash", "prod", "Allow", "", "nightly", "")
	if n := countRuns(t, pool, "skipper", "queued"); n != 1 {
		t.Errorf("dangling skip binding enqueued %d, want 1 (fail open — a visible policy violation)", n)
	}

	seedJobRow(t, pool, "onlyer", 1)
	bindSchedule(t, pool, "job", "onlyer", "nightly", "", `["nonexistent"]`)
	s.fire("git", "onlyer", "", "bash", "prod", "Allow", "", "nightly", "")
	if n := countRuns(t, pool, "onlyer", "queued"); n != 0 {
		t.Errorf("dangling only binding enqueued %d, want 0 (fail closed — silence is what it asked for)", n)
	}
}

// An entry with no bindings and no global calendars must be completely
// unaffected — the upgrade no-op guarantee.
func TestUnboundEntryIsUnaffected(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "plain", 1)
	bindSchedule(t, pool, "job", "plain", "nightly", "", "")
	s.fire("git", "plain", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "plain", "queued"); n != 1 {
		t.Errorf("enqueued %d, want 1", n)
	}
	if n := countRuns(t, pool, "plain", "skipped"); n != 0 {
		t.Errorf("recorded %d skips, want 0", n)
	}
}
