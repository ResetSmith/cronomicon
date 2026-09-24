package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// SLA monitoring and the missed-run detector (SL,
// the prod-features plan §3).
//
// Two scans that answer questions History could not:
//
//   - OVERDUE — this run is still going past the deadline its job declares.
//     Distinct from timeout_seconds, which KILLS. A backup still running at 6am
//     may be perfectly healthy and merely slow; the right response is a person,
//     not a SIGKILL. It also cannot reuse timeout's mechanism: all three of its
//     enforcement points are in-process context deadlines held by whichever
//     executor owns the run, and for a runner-executed run the server holds no
//     goroutine at all. So this is a server-side scan, which additionally
//     survives a restart.
//
//   - MISSED — a schedule expected a fire and no run appeared. The scheduler
//     already computes next-fire times for the Score's hollow marks; this is the
//     retrospective twin.
//
// Both follow the reactor's template rather than inventing one: a settings-KV
// cursor, a bounded catch-up, and a FIRST PASS THAT ANCHORS AND SCANS NOTHING,
// so upgrading to this release cannot page anyone about last month.

const (
	// slaScanInterval — a minute is the resolution of the deadlines themselves
	// (must_finish_by is HH:MM), so scanning faster buys nothing.
	slaScanInterval = time.Minute

	// missedScanInterval is slower: the detector reads schedule specs and
	// enumerates fire times, which is more work than probing running runs, and a
	// missed fire is not more urgent for being noticed 60 seconds sooner.
	missedScanInterval = 5 * time.Minute

	// missedGrace is how long after an expected fire the detector waits before
	// calling it missed. Enqueueing is a single INSERT on the cron tick, so a
	// minute is already generous; five absorbs a slow reload or a busy pool
	// without ever calling a real fire missing.
	missedGrace = 5 * time.Minute

	// missedCatchUp bounds how far back a scan will look, mirroring
	// reactionMissGrace for the same reason: a Monday restart after a weekend
	// outage must not deliver two days of alerts at once.
	missedCatchUp = 24 * time.Hour

	settingsKeyMissedCursor = "slaMissedCursor"
)

// reasonMissedFire is the queued_reason recorded for a silent miss. It is one of
// the dedupeEpisodeReason texts, so it is load-bearing — see skipRecord.Reason.
const reasonMissedFire = "Missed: the schedule expected a fire and no run appeared"

// SetNotifier installs the alert sink. Nil (the default) disables SL alerting
// entirely, which is what keeps every existing test silent.
func (s *Scheduler) SetNotifier(n notify.Notifier) {
	s.mu.Lock()
	s.notifier = n
	s.mu.Unlock()
}

func (s *Scheduler) notifierRef() notify.Notifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notifier
}

// slaLoop ticks the overdue scan.
func (s *Scheduler) slaLoop(ctx context.Context) {
	t := time.NewTicker(slaScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScanSLA(ctx)
		}
	}
}

// missedLoop ticks the missed-run detector.
func (s *Scheduler) missedLoop(ctx context.Context) {
	t := time.NewTicker(missedScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScanMissedRuns(ctx)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Overdue runs
// ─────────────────────────────────────────────────────────────────────────────

// ScanSLA warns once for each running run that has passed its job's declared
// deadline. Exported so tests can drive one deterministic pass.
//
// It never touches the run. Killing is timeout_seconds' job and stays there.
func (s *Scheduler) ScanSLA(ctx context.Context) {
	now := time.Now().UTC()
	loc := s.location()

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.job_name, COALESCE(r.job_source,'git'), COALESCE(r.scope,''), r.started_at,
		       j.warn_after_seconds, j.must_finish_by
		  FROM runs r
		  JOIN jobs j ON j.name = r.job_name AND j.source = COALESCE(r.job_source,'git')
		 WHERE r.status = 'running'
		   AND r.sla_warned_at IS NULL
		   AND r.started_at IS NOT NULL
		   AND (j.warn_after_seconds IS NOT NULL OR j.must_finish_by IS NOT NULL)`)
	if err != nil {
		s.log.Error("sla: scan running runs", "err", err)
		return
	}
	type breach struct{ id, job, source, scope, detail string }
	var breaches []breach
	for rows.Next() {
		var id, job, source, scope, startedAt string
		var warnAfter sql.NullInt64
		var mustFinishBy sql.NullString
		if err := rows.Scan(&id, &job, &source, &scope, &startedAt, &warnAfter, &mustFinishBy); err != nil {
			s.log.Error("sla: scan row", "err", err)
			continue
		}
		started := parseStamp(startedAt)
		if started.IsZero() {
			continue
		}
		if d := slaBreachDetail(started, now, loc, warnAfter, mustFinishBy); d != "" {
			breaches = append(breaches, breach{id, job, source, scope, d})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.log.Error("sla: iterate running runs", "err", err)
	}

	for _, b := range breaches {
		// Stamp FIRST and only alert if this pass is the one that claimed it.
		// Two schedulers (or a scan overlapping a slow send) must not both page.
		res, err := s.db.ExecContext(ctx,
			`UPDATE runs SET sla_warned_at = ? WHERE id = ? AND sla_warned_at IS NULL`,
			now.Format(time.RFC3339), b.id)
		if err != nil {
			s.log.Error("sla: stamp warned", "run", b.id, "err", err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		s.log.Warn("sla: run is overdue", "run", b.id, "job", b.job, "detail", b.detail)
		s.raiseAlert(ctx, notify.AlertEvent{
			Trigger: notify.TriggerSLABreach,
			JobName: b.job,
			Scope:   b.scope,
			TraceID: b.id,
			Subject: "[Cronomicon] " + b.job + " is running late",
			Detail:  b.detail,
		}, "SLA breach", "overdue")
	}
}

// slaBreachDetail returns the human sentence for a breach, or "" when the run is
// still within both deadlines.
//
// must_finish_by is resolved in the APPLICATION zone, never UTC — the operator
// typed a local business hour, and CAL-7 established that computing such a thing
// in UTC is the bug the feature exists to avoid. A deadline earlier in the day
// than the run's start is read as "tomorrow", which is what makes an overnight
// window ("must be done by 06:00", started 22:00) mean what it looks like.
func slaBreachDetail(started, now time.Time, loc *time.Location, warnAfter sql.NullInt64, mustFinishBy sql.NullString) string {
	ran := now.Sub(started)
	if warnAfter.Valid && warnAfter.Int64 > 0 && ran > time.Duration(warnAfter.Int64)*time.Second {
		return fmt.Sprintf("Running for %s, past its expected %s.",
			ran.Round(time.Second), (time.Duration(warnAfter.Int64) * time.Second).String())
	}
	if mustFinishBy.Valid {
		if deadline, ok := resolveDeadline(started, loc, mustFinishBy.String); ok && now.After(deadline) {
			return fmt.Sprintf("Still running at %s, past its %s deadline (started %s, %s ago).",
				now.In(loc).Format("15:04"), mustFinishBy.String,
				started.In(loc).Format("15:04"), ran.Round(time.Second))
		}
	}
	return ""
}

// resolveDeadline turns an 'HH:MM' wall-clock into the next such instant at or
// after the run's start, in loc.
func resolveDeadline(started time.Time, loc *time.Location, hhmm string) (time.Time, bool) {
	h, m, ok := cronutil.ParseDeadline(hhmm)
	if !ok {
		return time.Time{}, false
	}
	local := started.In(loc)
	d := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc)
	if !d.After(local) {
		d = d.AddDate(0, 0, 1) // the deadline is tomorrow's — an overnight window
	}
	return d.UTC(), true
}

// ─────────────────────────────────────────────────────────────────────────────
// Missed runs
// ─────────────────────────────────────────────────────────────────────────────

// ScanMissedRuns looks backwards: for every fire each schedule entry SHOULD have
// produced in the window just closed, was there a run row? Exported so tests can
// drive one deterministic pass.
//
// # Why this errs toward silence
//
// A fire is reported missing only when nothing explains it. "Explained" is
// deliberately generous: any run row near the expected time, OR any suppression
// recorded for that definition and entry on the same day, OR the definition
// being paused or disabled or binned right now.
//
// The day-wide suppression clause is doing real work. Calendar skips de-dupe per
// DAY (CAL-8's dedupeDay), so a `*/5` entry suppressed across a holiday leaves
// ONE row for 288 expected fires — a detector that demanded a row per fire would
// report 287 misses for a job that behaved exactly as configured. Pause and cap
// skips de-dupe per EPISODE for the same reason. Rather than reconstruct each
// suppression's extent, the rule treats a day with any suppression on record as
// accounted for.
//
// The cost is a real miss going unreported on a day that also had a suppression.
// That is the right trade for a first version: a false 3am page destroys trust
// in an alert far faster than a late one, and the analytics view shows the gap
// either way.
func (s *Scheduler) ScanMissedRuns(ctx context.Context) {
	now := time.Now().UTC()
	windowEnd := now.Add(-missedGrace)

	cursor, ok := s.missedCursor(ctx)
	if !ok {
		// First pass EVER: anchor and scan nothing. Copied from the reactor, and
		// for the sharper reason — without it, deploying this release would
		// enumerate every fire since the epoch and alert on all of them.
		s.writeMissedCursor(ctx, windowEnd)
		s.log.Info("sla: missed-run detector anchored; no retrospective scan")
		return
	}
	if cursor.Before(windowEnd.Add(-missedCatchUp)) {
		s.log.Warn("sla: missed-run cursor is stale; skipping the gap rather than alerting on it",
			"cursor", cursor.Format(time.RFC3339), "cappedTo", missedCatchUp.String())
		cursor = windowEnd.Add(-missedCatchUp)
	}
	if !windowEnd.After(cursor) {
		return // nothing has aged past the grace window yet
	}

	loc := s.location()
	entries, err := s.scheduleEntriesForDetection(ctx)
	if err != nil {
		s.log.Error("sla: load schedule entries", "err", err)
		return
	}

	for _, e := range entries {
		// ParseSpec is the SAME resolver the engine fires by (and the Score
		// projects by), so "expected" here cannot drift from "actual" — including
		// activation windows, which must be honoured or every fire outside a
		// window would read as missing.
		sched, err := cronutil.ParseSpec(cronutil.Spec{
			Cron:     e.cron,
			Interval: e.interval,
			Window:   parseWindowCols(nullStr(e.startAt), nullStr(e.endAt)),
		})
		if err != nil || sched == nil {
			continue // an unparseable entry is the reload's problem to report, not this scan's
		}
		// Enumerate in the APP ZONE, not UTC. A parsed cron carries no location
		// of its own, so Next() adopts the zone of the instant it is handed —
		// and the engine hands it one in s.location() (cron.WithLocation). A UTC
		// cursor therefore enumerates a DIFFERENT set of instants: for a
		// New_York app zone, "0 2 * * *" resolves to 02:00Z rather than the
		// 06:00Z the engine fires at, so every real fire reads as missing and
		// every enumerated one as a phantom miss. Absolute comparisons
		// (Before/After) are zone-independent; only the enumeration is not.
		for t := sched.Next(cursor.In(loc)); !t.IsZero() && t.Before(windowEnd); t = sched.Next(t) {
			fired := t.UTC()
			explained, err := s.fireExplained(ctx, e, fired, loc)
			if err != nil {
				s.log.Error("sla: check expected fire", "job", e.ownerName, "err", err)
				continue
			}
			if explained {
				continue
			}
			s.recordMissedFire(ctx, e, fired)
		}
	}
	s.writeMissedCursor(ctx, windowEnd)
}

// detectionEntry is one schedule entry the detector watches.
type detectionEntry struct {
	ownerKind, ownerSource, ownerName string
	name, cron, interval              string
	startAt, endAt                    string
	scope                             string
}

// scheduleEntriesForDetection returns every entry belonging to a live, enabled,
// unpaused, unbinned definition. Anything excluded here is a fire that was never
// expected in the first place, which is cheaper to filter than to explain later.
func (s *Scheduler) scheduleEntriesForDetection(ctx context.Context) ([]detectionEntry, error) {
	// FX-B3 — workflows are watched too. A workflow schedule that silently stops
	// firing was previously undetectable: the suppression half already existed
	// (recordSkippedWorkflowFire) but nothing ever asked whether a workflow fire
	// was missing. The two arms are deliberately separate SELECTs rather than a
	// join over a CASE, because the owner tables share no scope column and a
	// single query would have to pretend they do.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ds.owner_kind, ds.owner_source, ds.owner_name, ds.name,
		       COALESCE(ds.cron,''), COALESCE(ds.interval,''),
		       COALESCE(ds.start_at,''), COALESCE(ds.end_at,''),
		       COALESCE(j.scope,'')
		  FROM definition_schedules ds
		  JOIN jobs j ON j.name = ds.owner_name AND j.source = ds.owner_source
		 WHERE ds.owner_kind = 'job' AND j.enabled = 1 AND j.deleted_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM paused_jobs p
		                    WHERE p.source = ds.owner_source AND p.owner_kind = 'job' AND p.name = ds.owner_name)
		UNION ALL
		SELECT ds.owner_kind, ds.owner_source, ds.owner_name, ds.name,
		       COALESCE(ds.cron,''), COALESCE(ds.interval,''),
		       COALESCE(ds.start_at,''), COALESCE(ds.end_at,''),
		       ''
		  FROM definition_schedules ds
		  JOIN workflows w ON w.name = ds.owner_name AND w.source = ds.owner_source
		 WHERE ds.owner_kind = 'workflow' AND w.enabled = 1 AND w.deleted_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM paused_jobs p
		                    WHERE p.source = ds.owner_source AND p.owner_kind = 'workflow' AND p.name = ds.owner_name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectionEntry
	for rows.Next() {
		var e detectionEntry
		if err := rows.Scan(&e.ownerKind, &e.ownerSource, &e.ownerName, &e.name,
			&e.cron, &e.interval, &e.startAt, &e.endAt, &e.scope); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// fireExplained reports whether anything accounts for the fire expected at t.
func (s *Scheduler) fireExplained(ctx context.Context, e detectionEntry, t time.Time, loc *time.Location) (bool, error) {
	if e.ownerKind == "workflow" {
		return s.workflowFireExplained(ctx, e, t, loc)
	}
	// 1. A run row near the expected instant — any status, including the skipped
	//    rows the calendar/pause/cap/Forbid gates write. The window opens a minute
	//    early to absorb clock jitter between the cron engine and the INSERT.
	from := t.Add(-time.Minute).Format(time.RFC3339)
	to := t.Add(missedGrace).Format(time.RFC3339)
	var n int
	// FX-B1 — scheduled_for is checked alongside created_at, because a fire the
	// Queue policy parked and promoted LATE has a created_at at promotion time,
	// outside this window. Promotion deletes the pending row check 2 would have
	// found, so between the two there was a gap exactly as wide as the gate was
	// held past the grace: the detector paged for a run that had in fact executed
	// moments earlier, and left a fabricated 'skipped' row to explain away every
	// later fire that day. scheduled_for is the fire instant carried across that
	// delete, so the promoted run is recognisable as the fire it was.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs
		 WHERE job_name = ? AND COALESCE(job_source,'git') = ?
		   AND COALESCE(schedule_name,'') = ?
		   AND ((created_at   >= ? AND created_at   <= ?)
		     OR (scheduled_for >= ? AND scheduled_for <= ?))`,
		e.ownerName, e.ownerSource, e.name, from, to, from, to).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}

	// 2. A fire parked by the Queue concurrency policy. A queued fire writes no
	//    runs row at all — only a pending_runs row that promotes when the gate
	//    clears (queue.go) — so without this it reads as a silent miss, pages,
	//    and leaves a fabricated 'skipped' row that would then explain every
	//    later fire that day. The row's created_at IS the fire instant, so the
	//    same window applies. A fire that promotes before the scan is already
	//    explained by check 1; one that has not promoted is still parked here.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_runs
		 WHERE kind = 'job' AND name = ? AND source = ?
		   AND gate_kind = 'concurrency' AND status = 'pending'
		   AND COALESCE(json_extract(params_json,'$.ScheduleName'),'') = ?
		   AND created_at >= ? AND created_at <= ?`,
		e.ownerName, e.ownerSource, e.name, from, to).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}

	// 3. Any suppression recorded for this definition + entry on the same
	//    application-zone day. See the function comment: suppression records
	//    de-dupe per day or per episode, so one row can legitimately stand for
	//    hundreds of expected fires.
	day := calendar.DayOf(t, loc)
	rows, err := s.db.QueryContext(ctx, `
		SELECT created_at FROM runs
		 WHERE job_name = ? AND COALESCE(job_source,'git') = ?
		   AND COALESCE(schedule_name,'') = ?
		   AND status = 'skipped'
		 ORDER BY created_at DESC LIMIT 200`,
		e.ownerName, e.ownerSource, e.name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt string
		if err := rows.Scan(&createdAt); err != nil {
			return false, err
		}
		if calendar.DayOf(parseStamp(createdAt), loc) == day {
			return true, nil
		}
	}
	return false, rows.Err()
}

// workflowFireExplained is fireExplained's workflow arm (FX-B3).
//
// The same three questions asked of workflow_runs. There is no pending_runs
// check because a workflow fire is never gate-queued: the Queue concurrency
// policy is a job-spec field, and a workflow has no concurrency key of its own —
// which is why this arm is shorter rather than merely different.
func (s *Scheduler) workflowFireExplained(ctx context.Context, e detectionEntry, t time.Time, loc *time.Location) (bool, error) {
	from := t.Add(-time.Minute).Format(time.RFC3339)
	to := t.Add(missedGrace).Format(time.RFC3339)
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_runs
		 WHERE workflow_name = ? AND COALESCE(workflow_source,'git') = ?
		   AND COALESCE(schedule_name,'') = ?
		   AND created_at >= ? AND created_at <= ?`,
		e.ownerName, e.ownerSource, e.name, from, to).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}

	day := calendar.DayOf(t, loc)
	rows, err := s.db.QueryContext(ctx, `
		SELECT created_at FROM workflow_runs
		 WHERE workflow_name = ? AND COALESCE(workflow_source,'git') = ?
		   AND COALESCE(schedule_name,'') = ?
		   AND status = 'skipped'
		 ORDER BY created_at DESC LIMIT 200`,
		e.ownerName, e.ownerSource, e.name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt string
		if err := rows.Scan(&createdAt); err != nil {
			return false, err
		}
		if calendar.DayOf(parseStamp(createdAt), loc) == day {
			return true, nil
		}
	}
	return false, rows.Err()
}

// recordMissedFire leaves the durable row and raises the alert.
//
// The row is a terminal 'skipped' run stamped at the EXPECTED fire time rather
// than at detection time, so History shows the gap where it actually happened.
// It reuses the suppression vocabulary deliberately: an operator asking "what
// happened at 02:00?" should find one kind of answer, not two.
func (s *Scheduler) recordMissedFire(ctx context.Context, e detectionEntry, t time.Time) {
	if e.ownerKind == "workflow" {
		s.recordMissedWorkflowFire(ctx, e, t)
		return
	}
	err := s.recordSuppression(ctx, EnqueueParams{
		JobName: e.ownerName, JobSource: e.ownerSource, Scope: e.scope,
		ScheduleName: e.name,
		RunType:      s.runTypeOf(ctx, e.ownerSource, e.ownerName),
	}, skipRecord{
		Reason: reasonMissedFire,
		Mode:   dedupeEpisodeReason,
		At:     t.UTC().Format(time.RFC3339),
	})
	if err != nil {
		s.log.Error("sla: record missed fire", "job", e.ownerName, "err", err)
		return
	}
	s.log.Warn("sla: expected fire did not happen",
		"job", e.ownerName, "source", e.ownerSource, "schedule", e.name, "expected", t.Format(time.RFC3339))
	s.raiseAlert(ctx, notify.AlertEvent{
		Trigger: notify.TriggerMissedRun,
		JobName: e.ownerName,
		Scope:   e.scope,
		Subject: "[Cronomicon] " + e.ownerName + " did not run",
		Detail: fmt.Sprintf("Schedule %q expected a run at %s and none appeared. "+
			"No calendar, pause or concurrency suppression was recorded to explain it.",
			e.name, t.In(s.location()).Format(time.RFC3339)),
	}, "Missed run", "missed")
}

// recordMissedWorkflowFire is recordMissedFire's workflow arm (FX-B3): the same
// durable marker and the same alert, written to workflow_runs.
func (s *Scheduler) recordMissedWorkflowFire(ctx context.Context, e detectionEntry, t time.Time) {
	err := s.recordWorkflowSuppression(ctx, e.ownerSource, e.ownerName, e.name, skipRecord{
		Reason: reasonMissedFire,
		Mode:   dedupeEpisodeReason,
		At:     t.UTC().Format(time.RFC3339),
	})
	if err != nil {
		s.log.Error("sla: record missed workflow fire", "workflow", e.ownerName, "err", err)
		return
	}
	s.log.Warn("sla: expected workflow fire did not happen",
		"workflow", e.ownerName, "source", e.ownerSource, "schedule", e.name, "expected", t.Format(time.RFC3339))
	s.raiseAlert(ctx, notify.AlertEvent{
		Trigger:   notify.TriggerMissedRun,
		JobName:   e.ownerName,
		OwnerKind: "workflow",
		Subject:   "[Cronomicon] workflow " + e.ownerName + " did not run",
		Detail: fmt.Sprintf("Workflow schedule %q expected a run at %s and none appeared. "+
			"No calendar or pause suppression was recorded to explain it.",
			e.name, t.In(s.location()).Format(time.RFC3339)),
	}, "Missed run", "missed")
}

func (s *Scheduler) runTypeOf(ctx context.Context, source, name string) string {
	var rt string
	if err := s.db.QueryRowContext(ctx,
		`SELECT run_type FROM jobs WHERE source = ? AND name = ?`, source, name).Scan(&rt); err != nil || rt == "" {
		return "bash"
	}
	return rt
}

// raiseAlert emits the notification and the Activity row together.
//
// Activity matters as much as the notification: an install with no SMTP or
// Apprise configured must still be able to SEE that an SLA fired, or the feature
// is invisible to exactly the operators most likely to be running without a
// mail relay.
func (s *Scheduler) raiseAlert(ctx context.Context, ev notify.AlertEvent, action, metricKind string) {
	metrics.SLABreach(metricKind)
	if n := s.notifierRef(); n != nil {
		n.AlertRaised(ev)
	}
	summary := ev.Subject
	if ev.Detail != "" {
		summary = ev.Detail
	}
	// Outcome is "warning", not "failure": nothing failed. A job running long is
	// a deadline missed, and a fire that never happened may well be a scheduler
	// that was down — neither is the job reporting an error, and colouring them
	// red would put a permanent failure in a catalog that is otherwise green.
	// Same instinct as the reaction depth-ceiling activity ("a brake engaged").
	if err := auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		Kind:     "config",
		Outcome:  "warning",
		Actor:    "scheduler",
		JobName:  ev.JobName,
		Scope:    ev.Scope,
		TraceID:  ev.TraceID,
		Category: "Jobs",
		Action:   action,
		Target:   ev.JobName,
		Summary:  summary,
	}); err != nil {
		s.log.Error("sla: record alert activity", "job", ev.JobName, "err", err)
	}
}

func (s *Scheduler) missedCursor(ctx context.Context) (time.Time, bool) {
	var val sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, settingsKeyMissedCursor).Scan(&val); err != nil {
		return time.Time{}, false
	}
	if !val.Valid || val.String == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, val.String)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (s *Scheduler) writeMissedCursor(ctx context.Context, t time.Time) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingsKeyMissedCursor, t.UTC().Format(time.RFC3339)); err != nil {
		s.log.Error("sla: write missed cursor", "err", err)
	}
}
