package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// SL — the overdue scan and the missed-run detector.
//
// Both are alerting features, and the failure mode that destroys an alerting
// feature is not silence — it is noise. So most of what is pinned here is the
// scans declining to fire: once per run rather than once per minute, nothing on
// the pass that anchors the cursor, and nothing on a day whose suppression is
// already on record.

type captureAlerts struct{ events []notify.AlertEvent }

func (c *captureAlerts) RunEnded(notify.RunEvent)         {}
func (c *captureAlerts) AlertRaised(ev notify.AlertEvent) { c.events = append(c.events, ev) }

func seedSLAJob(t *testing.T, pool *sql.DB, name string, warnAfter *int, mustFinishBy string) {
	t.Helper()
	var wa any
	if warnAfter != nil {
		wa = *warnAfter
	}
	var mf any
	if mustFinishBy != "" {
		mf = mustFinishBy
	}
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, enabled, synced_at,
		                   warn_after_seconds, must_finish_by)VALUES ('uid-'||?, ?, 'git', 'bash', 'prod', 'Allow', 1, 't', ?, ?)`, name, name, wa, mf); err != nil {
		t.Fatalf("seed job %s: %v", name, err)
	}
}

func seedSLARun(t *testing.T, pool *sql.DB, id, job string, startedAgo time.Duration) {
	t.Helper()
	started := time.Now().UTC().Add(-startedAgo).Format(time.RFC3339)
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO runs (id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind, started_at, created_at)
		 VALUES (?, ?, 'git', 'bash', 'prod', 'running', 'scheduler', 'scheduled', ?, ?)`,
		id, job, started, started); err != nil {
		t.Fatalf("seed running run: %v", err)
	}
}

//go:fix inline

// A run past warn_after_seconds warns — once, however many times the scan runs.
func TestScanSLAWarnsOncePerRun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedSLAJob(t, pool, "slow", new(60), "")
	seedSLARun(t, pool, "run-1", "slow", 5*time.Minute)

	s.ScanSLA(context.Background())
	s.ScanSLA(context.Background())
	s.ScanSLA(context.Background())

	if len(alerts.events) != 1 {
		t.Fatalf("raised %d alerts across three scans, want 1 — an alert that repeats every minute is one an operator filters away: %+v", len(alerts.events), alerts.events)
	}
	ev := alerts.events[0]
	if ev.Trigger != notify.TriggerSLABreach {
		t.Errorf("trigger = %q, want %q", ev.Trigger, notify.TriggerSLABreach)
	}
	if ev.JobName != "slow" || ev.TraceID != "run-1" || ev.Scope != "prod" {
		t.Errorf("alert identity = %+v, want job slow / run-1 / prod", ev)
	}

	// The run is untouched — killing is timeout_seconds' job and stays there.
	var status string
	_ = pool.QueryRow(`SELECT status FROM runs WHERE id='run-1'`).Scan(&status)
	if status != "running" {
		t.Errorf("run status = %q, want running — the SLA scan must never touch the run", status)
	}
	var warned sql.NullString
	_ = pool.QueryRow(`SELECT sla_warned_at FROM runs WHERE id='run-1'`).Scan(&warned)
	if !warned.Valid {
		t.Error("sla_warned_at was not stamped, so the next scan would alert again")
	}
}

// A job with no deadline, and a run inside its deadline, both stay silent.
func TestScanSLAStaysSilentWithoutABreach(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedSLAJob(t, pool, "no-deadline", nil, "")
	seedSLAJob(t, pool, "within", new(3600), "")
	seedSLARun(t, pool, "run-a", "no-deadline", 24*time.Hour)
	seedSLARun(t, pool, "run-b", "within", time.Minute)

	s.ScanSLA(context.Background())

	if len(alerts.events) != 0 {
		t.Errorf("raised %d alerts, want 0: %+v", len(alerts.events), alerts.events)
	}
}

// The wall-clock deadline is resolved in the application zone, and a deadline
// earlier in the day than the start means tomorrow — which is what makes an
// overnight window ("done by 06:00", started 22:00) behave.
func TestResolveDeadlineHandlesOvernightWindows(t *testing.T) {
	loc := time.UTC
	started := time.Date(2026, 8, 11, 22, 0, 0, 0, loc)

	d, ok := resolveDeadline(started, loc, "06:00")
	if !ok {
		t.Fatal("06:00 did not parse")
	}
	if !d.After(started) {
		t.Errorf("deadline %s is not after the 22:00 start — an overnight window resolved backwards", d)
	}
	if d.Day() != 12 {
		t.Errorf("deadline day = %d, want 12 (tomorrow)", d.Day())
	}

	// A deadline later the same day stays today.
	morning := time.Date(2026, 8, 11, 1, 0, 0, 0, loc)
	d2, _ := resolveDeadline(morning, loc, "06:00")
	if d2.Day() != 11 {
		t.Errorf("deadline day = %d, want 11 (same day)", d2.Day())
	}

	if _, ok := resolveDeadline(started, loc, "25:00"); ok {
		t.Error("25:00 parsed as a deadline")
	}
	if _, ok := resolveDeadline(started, loc, "nonsense"); ok {
		t.Error("garbage parsed as a deadline")
	}
}

// ─── missed-run detector ─────────────────────────────────────────────────────

// The first pass ever must anchor and scan NOTHING. Without this, deploying the
// release enumerates every fire since the job was created and alerts on all of
// them.
func TestMissedDetectorFirstPassAnchorsSilently(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedJobRow(t, pool, "nightly", 1)
	seedSchedule(t, pool, "job", "nightly", "default", "* * * * *", 0)

	s.ScanMissedRuns(context.Background())

	if len(alerts.events) != 0 {
		t.Errorf("the anchoring pass raised %d alerts, want 0 — an upgrade must not page about history", len(alerts.events))
	}
	var cursor string
	if err := pool.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingsKeyMissedCursor).Scan(&cursor); err != nil {
		t.Fatalf("the first pass did not anchor a cursor: %v", err)
	}
}

// With the cursor already behind, a per-minute schedule that produced no runs
// is reported — once per expected fire, with a durable row stamped at the time
// the fire was expected rather than at detection.
func TestMissedDetectorReportsSilentMisses(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedJobRow(t, pool, "nightly", 1)
	seedSchedule(t, pool, "job", "nightly", "default", "* * * * *", 0)

	// Anchor the cursor 20 minutes back so a handful of fires have aged past the
	// grace window.
	back := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, back); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	s.ScanMissedRuns(context.Background())

	if len(alerts.events) == 0 {
		t.Fatal("a per-minute schedule with no runs at all raised nothing")
	}
	for _, ev := range alerts.events {
		if ev.Trigger != notify.TriggerMissedRun {
			t.Errorf("trigger = %q, want %q", ev.Trigger, notify.TriggerMissedRun)
		}
		if ev.TraceID != "" {
			t.Errorf("a missed run carries TraceID %q — there is no run, which is the point", ev.TraceID)
		}
	}
	// Durable rows, stamped in the past.
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='nightly' AND status='skipped'`).Scan(&n)
	if n == 0 {
		t.Error("no durable row was recorded for the misses")
	}
	var createdAt string
	_ = pool.QueryRow(`SELECT created_at FROM runs WHERE job_name='nightly' AND status='skipped' ORDER BY created_at LIMIT 1`).Scan(&createdAt)
	if t0 := parseStamp(createdAt); t0.IsZero() || time.Since(t0) < 5*time.Minute {
		t.Errorf("the missed row is stamped %q — it should carry the EXPECTED fire time, not detection time", createdAt)
	}

	// A second pass must not re-report the same window.
	before := len(alerts.events)
	s.ScanMissedRuns(context.Background())
	if len(alerts.events) != before {
		t.Errorf("a second pass re-reported the same window (%d → %d)", before, len(alerts.events))
	}
}

// A fire that actually produced a run is not a miss.
func TestMissedDetectorIgnoresFiresThatRan(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedJobRow(t, pool, "hourly", 1)
	seedSchedule(t, pool, "job", "hourly", "default", "* * * * *", 0)

	// A run for every minute in the window under scrutiny.
	now := time.Now().UTC()
	for i := 6; i <= 20; i++ {
		at := now.Add(-time.Duration(i) * time.Minute).Truncate(time.Minute)
		if _, err := pool.Exec(
			`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, schedule_name, created_at)
			 VALUES (?, 'hourly', 'git', 'bash', 'success', 'scheduler', 'scheduled', 'default', ?)`,
			"r"+at.Format("150405"), at.Format(time.RFC3339)); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
	back := now.Add(-20 * time.Minute).Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, back); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	s.ScanMissedRuns(context.Background())

	if len(alerts.events) != 0 {
		t.Errorf("raised %d alerts for fires that all ran: %+v", len(alerts.events), alerts.events)
	}
}

// The noise guard that matters most: a calendar veto de-dupes per DAY, so one
// skipped row legitimately stands for every fire that day. A detector demanding
// a row per fire would report 287 misses for a `*/5` job that behaved exactly
// as configured.
func TestMissedDetectorTreatsASuppressedDayAsExplained(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedJobRow(t, pool, "holiday-job", 1)
	seedSchedule(t, pool, "job", "holiday-job", "default", "* * * * *", 0)

	// ONE calendar-suppression row for today, as dedupeDay would leave.
	if _, err := pool.Exec(
		`INSERT INTO runs (id, job_name, job_source, run_type, status, queued_reason, suppressed_by_calendar,
		                   triggered_by, trigger_kind, schedule_name, created_at)
		 VALUES ('cal-1', 'holiday-job', 'git', 'bash', 'skipped', 'Skipped: Independence Day', 'federal-holidays',
		         'scheduler', 'scheduled', 'default', ?)`,
		time.Now().UTC().Add(-30*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed calendar skip: %v", err)
	}
	back := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, back); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	s.ScanMissedRuns(context.Background())

	if len(alerts.events) != 0 {
		t.Errorf("raised %d alerts on a day with a suppression already on record — "+
			"one row stands for the whole day (CAL-8 dedupeDay): %+v", len(alerts.events), alerts.events)
	}
}

// dailyCronAt renders a cron expression that fires once a day, at t.
//
// The fields are rendered in the APP ZONE, because that is the zone both the
// engine and the scan read them in: an untagged expression carries no location
// of its own, so its schedule adopts the zone of the instant it is asked about,
// and both askers use s.location(). Rendering in UTC instead would name an
// instant an offset away for any app zone but UTC — which is exactly the bug
// the scan's cursor.In(loc) conversion fixes, so a test that rendered in UTC
// would be asserting against the defect rather than the behaviour.
func dailyCronAt(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	return fmt.Sprintf("%d %d * * *", local.Minute(), local.Hour())
}

// QP × SL — a fire the Queue policy PARKED is not a miss.
//
// A queued fire writes no runs row at all: it lives in pending_runs until the
// gate clears (queue.go). To a detector that only looks at `runs` that is
// indistinguishable from a scheduler that had died, so it paged — and then left
// a fabricated 'skipped' row which, under the day-wide rule of check 3, went on
// to explain every LATER fire that day. Both halves are pinned here, against the
// contrast case that keeps the assertion honest: with nothing parked, the very
// same scan must still report the miss.
func TestMissedDetectorTreatsAQueueParkedFireAsExplained(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parked bool
	}{
		{"parked by Queue", true},
		{"the same fire with nothing parked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			alerts := &captureAlerts{}
			s.SetNotifier(alerts)

			// ONE expected fire, ten minutes back: old enough to have aged past the
			// grace window, young enough to sit inside the catch-up bound. A
			// per-minute entry would not do — the first miss recorded would explain
			// all the rest of the day's fires, and the counts would prove nothing.
			fireAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)

			seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
			seedSchedule(t, pool, "job", "patch", "nightly", dailyCronAt(fireAt, s.location()), 0)
			seedActiveKeyedRun(t, pool, "active-1", "patch", "git/patch")

			// The fire meets the held gate and parks: a pending_runs row, no run.
			s.fire("git", "patch", "", "bash", "prod", cronutil.PolicyQueue, "git/patch", "nightly", "")
			if d := queueDepth(t, pool, "git/patch"); d != 1 {
				t.Fatalf("setup: queue depth = %d, want 1", d)
			}
			if n := countRuns(t, pool, "patch", "queued"); n != 0 {
				t.Fatalf("setup: the parked fire enqueued %d runs — there must be no runs row for the detector to find", n)
			}
			// TryQueue stamps created_at at wall-clock now; the fire it stands for is
			// the one the scan is about to enumerate.
			if _, err := pool.Exec(
				`UPDATE pending_runs SET created_at = ? WHERE gate_kind='concurrency'`,
				fireAt.Format(time.RFC3339)); err != nil {
				t.Fatalf("backdate parked row: %v", err)
			}
			if !tc.parked {
				// The contrast case is the same world minus the parked row — that one
				// row is the entire difference between the two directions.
				if _, err := pool.Exec(`DELETE FROM pending_runs`); err != nil {
					t.Fatalf("drop parked row: %v", err)
				}
			}

			cursor := fireAt.Add(-time.Minute).Format(time.RFC3339)
			if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, cursor); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}

			s.ScanMissedRuns(context.Background())

			if tc.parked {
				if len(alerts.events) != 0 {
					t.Errorf("a parked fire raised %d missed-run alerts, want 0 — the fire was not lost, "+
						"it is waiting in pending_runs for the gate to clear: %+v", len(alerts.events), alerts.events)
				}
				if n := countRuns(t, pool, "patch", "skipped"); n != 0 {
					t.Errorf("the detector recorded %d 'skipped' rows for a fire that is still queued — "+
						"beyond the false page, one such row satisfies check 3 for the WHOLE day, so the next real miss goes unreported too", n)
				}
				// And the fire is still parked: explaining it must not consume it.
				if d := queueDepth(t, pool, "git/patch"); d != 1 {
					t.Errorf("queue depth after the scan = %d, want 1 — the detector must only read", d)
				}
				return
			}

			if len(alerts.events) != 1 {
				t.Fatalf("with nothing parked the scan raised %d alerts, want 1 — if this direction is silent, "+
					"the parked case above proves nothing: %+v", len(alerts.events), alerts.events)
			}
			if got := alerts.events[0].Trigger; got != notify.TriggerMissedRun {
				t.Errorf("trigger = %q, want %q", got, notify.TriggerMissedRun)
			}
			if got := skippedRows(t, pool, "patch"); len(got) != 1 || got[0] != reasonMissedFire {
				t.Errorf("durable rows = %v, want exactly one %q", got, reasonMissedFire)
			}
		})
	}
}

// A paused or binned definition expects no fires at all.
func TestMissedDetectorSkipsPausedDisabledAndBinnedDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, pool *sql.DB)
	}{
		{"paused", func(t *testing.T, pool *sql.DB) { pauseDef(t, pool, "git", "job", "quiet") }},
		{"disabled", func(t *testing.T, pool *sql.DB) {
			if _, err := pool.Exec(`UPDATE jobs SET enabled=0 WHERE name='quiet'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"binned", func(t *testing.T, pool *sql.DB) {
			if _, err := pool.Exec(`UPDATE jobs SET deleted_at='2026-08-11T00:00:00Z' WHERE name='quiet'`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			alerts := &captureAlerts{}
			s.SetNotifier(alerts)

			seedJobRow(t, pool, "quiet", 1)
			seedSchedule(t, pool, "job", "quiet", "default", "* * * * *", 0)
			tc.setup(t, pool)

			back := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
			if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, back); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}
			s.ScanMissedRuns(context.Background())

			if len(alerts.events) != 0 {
				t.Errorf("a %s definition raised %d missed-run alerts, want 0", tc.name, len(alerts.events))
			}
		})
	}
}

// A stale cursor (the Monday-after-a-weekend-outage case) is capped rather than
// discharged, mirroring the reactor's catch-up bound.
func TestMissedDetectorCapsAStaleCursor(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	seedJobRow(t, pool, "nightly", 1)
	seedSchedule(t, pool, "job", "nightly", "default", "0 * * * *", 0) // hourly

	// A cursor a week back would be 168 expected fires without the cap.
	weekAgo := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, weekAgo); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	s.ScanMissedRuns(context.Background())

	if len(alerts.events) > 25 {
		t.Errorf("a week-stale cursor produced %d alerts — the 24h catch-up bound did not hold", len(alerts.events))
	}
}

// SL — the detector must enumerate expected fires in the APP ZONE.
//
// An untagged cron expression carries no location, so its schedule adopts the
// zone of whatever instant it is handed. The engine hands it one in
// s.location() (cron.WithLocation), so that is the zone its fires happen in.
// ScanMissedRuns used to hand it a UTC cursor, which for any app zone but UTC
// enumerates a DIFFERENT set of instants — real fires never checked, phantom
// ones alerted on.
//
// Asia/Kolkata is chosen deliberately: its +05:30 offset moves BOTH the hour
// and the minute, so a test that only compared hours could not tell the two
// enumerations apart.
func TestMissedDetectorEnumeratesInTheAppZone(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	s.loc = kolkata
	alerts := &captureAlerts{}
	s.SetNotifier(alerts)

	fireAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	seedQueueJob(t, pool, "ledger", cronutil.PolicyAllow)
	seedSchedule(t, pool, "job", "ledger", "nightly", dailyCronAt(fireAt, kolkata), 0)

	cursor := fireAt.Add(-time.Minute).Format(time.RFC3339)
	if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, cursor); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	s.ScanMissedRuns(context.Background())

	// Nothing ran, so this fire IS missing — the point is that the scan looked
	// at the right instant to notice. Enumerated in UTC the expected fire lands
	// five and a half hours away, outside the window entirely, and the scan
	// reports nothing at all.
	if len(alerts.events) != 1 {
		t.Fatalf("raised %d missed-run alerts, want 1 — a non-UTC app zone must not "+
			"move the instants the detector checks: %+v", len(alerts.events), alerts.events)
	}
	var stamp string
	if err := pool.QueryRow(
		`SELECT created_at FROM runs WHERE job_name='ledger' AND status='skipped'`).Scan(&stamp); err != nil {
		t.Fatalf("read recorded miss: %v", err)
	}
	// The row is stamped at the fire it explains, not at discovery time, so
	// History shows the gap where it actually happened.
	if got := parseStamp(stamp).UTC(); !got.Equal(fireAt) {
		t.Errorf("miss stamped %s, want the expected fire instant %s",
			got.Format(time.RFC3339), fireAt.Format(time.RFC3339))
	}
}

// FX-B1 — a Queue-parked fire that PROMOTES LATE is not a miss either.
//
// v1.0.0 taught the detector to look in pending_runs, which covered the fire
// that is still parked. It did not cover the one that promoted a moment before
// the scan: promotion DELETES the pending row and stamps the new run's
// created_at at promotion time, so between check 1's window and check 2's row
// there was a gap exactly as wide as the gate was held past the grace. The
// detector paged for a run that had in fact just executed, and left a
// fabricated 'skipped' row that then explained away every later fire that day —
// the same day-wide poisoning the parked case was fixed to avoid.
//
// scheduled_for is the fire instant carried across that delete. The contrast
// case is a run with NO scheduled_for at the same promotion time, which must
// still be reported: without it this test would pass on a detector that had
// simply stopped looking.
func TestMissedDetectorTreatsALatePromotedFireAsExplained(t *testing.T) {
	for _, tc := range []struct {
		name          string
		carryInstant  bool
		wantAlerts    int
		wantSkipped   int
		failureDetail string
	}{
		{"promoted late, carrying its fire instant", true, 0, 0,
			"the run EXECUTED — paging for it is a false alarm, and the 'skipped' row it leaves behind explains away every later fire that day"},
		{"the same promotion without the instant", false, 1, 1,
			"a run whose origin cannot be established must still be reported, or this test proves nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			alerts := &captureAlerts{}
			s.SetNotifier(alerts)

			fireAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
			seedQueueJob(t, pool, "patch", cronutil.PolicyQueue)
			seedSchedule(t, pool, "job", "patch", "nightly", dailyCronAt(fireAt, s.location()), 0)

			// The world AFTER a late promotion: no pending row (promotion deleted
			// it) and a run created well outside the detector's [t-1m, t+5m]
			// window — here 8 minutes after the fire, i.e. past the 5m grace.
			promotedAt := fireAt.Add(8 * time.Minute)
			var scheduledFor any
			if tc.carryInstant {
				scheduledFor = fireAt.Format(time.RFC3339)
			}
			if _, err := pool.Exec(`
				INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind,
				                  schedule_name, created_at, scheduled_for)
				VALUES ('late-1', 'patch', 'git', 'bash', 'success', 'scheduler', 'scheduled', 'nightly', ?, ?)`,
				promotedAt.Format(time.RFC3339), scheduledFor); err != nil {
				t.Fatalf("seed promoted run: %v", err)
			}

			cursor := fireAt.Add(-time.Minute).Format(time.RFC3339)
			if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, cursor); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}

			s.ScanMissedRuns(context.Background())

			if len(alerts.events) != tc.wantAlerts {
				t.Errorf("missed-run alerts = %d, want %d — %s (events: %+v)",
					len(alerts.events), tc.wantAlerts, tc.failureDetail, alerts.events)
			}
			if n := countRuns(t, pool, "patch", "skipped"); n != tc.wantSkipped {
				t.Errorf("fabricated 'skipped' rows = %d, want %d — %s",
					n, tc.wantSkipped, tc.failureDetail)
			}
		})
	}
}

// FX-B3 — a WORKFLOW schedule that stops firing is detected too.
//
// The suppression half already existed (recordSkippedWorkflowFire) but nothing
// ever asked whether a workflow fire was missing, so a workflow schedule could
// go quiet indefinitely with no signal. The contrast case is the same scan with
// the run present, which must stay silent.
func TestMissedDetectorWatchesWorkflowSchedules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ranOnCue bool
		want     int
	}{
		{"the workflow never ran", false, 1},
		{"the workflow ran on cue", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			alerts := &captureAlerts{}
			s.SetNotifier(alerts)

			fireAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
			if _, err := pool.Exec(`
				INSERT INTO workflows (name, source, steps, synced_at)
				VALUES ('release', 'git', '[]', '2026-01-01T00:00:00Z')`); err != nil {
				t.Fatalf("seed workflow: %v", err)
			}
			seedSchedule(t, pool, "workflow", "release", "nightly", dailyCronAt(fireAt, s.location()), 0)

			if tc.ranOnCue {
				if _, err := pool.Exec(`
					INSERT INTO workflow_runs (id, workflow_name, workflow_source, status,
					                           triggered_by, trigger_kind, schedule_name, created_at)
					VALUES ('wf-1', 'release', 'git', 'success', 'scheduler', 'scheduled', 'nightly', ?)`,
					fireAt.Format(time.RFC3339)); err != nil {
					t.Fatalf("seed workflow run: %v", err)
				}
			}

			cursor := fireAt.Add(-time.Minute).Format(time.RFC3339)
			if _, err := pool.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, settingsKeyMissedCursor, cursor); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}

			s.ScanMissedRuns(context.Background())

			if len(alerts.events) != tc.want {
				t.Errorf("missed-run alerts = %d, want %d — a workflow schedule that stops "+
					"firing was previously undetectable (events: %+v)", len(alerts.events), tc.want, alerts.events)
			}
			var markers int
			_ = pool.QueryRow(
				`SELECT COUNT(*) FROM workflow_runs WHERE workflow_name='release' AND status='skipped'`).Scan(&markers)
			if markers != tc.want {
				t.Errorf("durable workflow markers = %d, want %d", markers, tc.want)
			}
		})
	}
}
