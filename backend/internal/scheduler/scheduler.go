// Package scheduler owns the cron scheduler and the run-enqueue logic.
//
// The scheduler reads the `jobs` table (populated by B3/gitlab), registers
// cron entries for jobs that have a schedule, and fires them by inserting a
// `runs` row with status='queued'.  B4 (runner) drains the queue; scheduler
// never imports B3, B4, or B6.
//
// Lifecycle: call New(...).Start(ctx); the orchestrator (main.go) is
// responsible for calling Start — this package exposes the seam but cannot
// self-register because it cannot import the main package.
//
// S16 concurrency: if a job has concurrency_policy='Forbid' and an active run
// (queued or running) shares the concurrency_key (default = job name), the
// scheduler skips that fire and logs the reason.
package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/envmerge"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

const (
	defaultMaxConcurrent = 5
	settingsKeyMax       = "maxConcurrent"
	// reloadBackstopInterval is the low-frequency fallback reload cadence; the
	// primary reload trigger is the onSyncComplete callback (ReloadIfChanged).
	reloadBackstopInterval = 5 * time.Minute
)

// WorkflowFirer dispatches a scheduled workflow. It is injected via
// SetWorkflowFirer so the scheduler need not import the workflow package
// (which itself imports scheduler).
type WorkflowFirer func(ctx context.Context, source, workflowName, scheduleName, envJSON string)

// Scheduler wraps the cron engine and the enqueue logic.
type Scheduler struct {
	db  *sql.DB
	log *slog.Logger
	cr  *cron.Cron

	mu                sync.Mutex
	loc               *time.Location       // effective app zone the engine fires in (never nil)
	loadedSHA         string               // git SHA the current cron entries were loaded from
	loadedFingerprint string               // DB fingerprint of the loaded job/workflow definitions
	reloads           int                  // count of reloads (for testing reload gating)
	wfFirer           WorkflowFirer        // set via SetWorkflowFirer; nil ⇒ workflows don't fire
	pendingWfFirer    PendingWorkflowFirer // set via SetPendingWorkflowFirer; nil ⇒ pending workflow rows retry
	notifier          notify.Notifier      // SL — set via SetNotifier; nil ⇒ no SLA/missed-run alerts
}

// newCronEngine builds a cron engine that fires in loc. robfig/cron fixes its
// location at construction (it cannot be re-zoned in place), so a zone change
// rebuilds the engine via RebuildWithLocation.
func newCronEngine(log *slog.Logger, loc *time.Location) *cron.Cron {
	return cron.New(
		cron.WithSeconds(), // 6-field expressions like GitLab CI; fall back to 5-field
		cron.WithLogger(cron.VerbosePrintfLogger(newCronLogger(log))),
		cron.WithLocation(loc),
	)
}

// New constructs a Scheduler firing in loc (the effective app zone, resolved at
// startup via settings.ResolveEffectiveTimezone). A nil loc degrades to
// time.Local. Call Start to begin scheduling.
func New(database *sql.DB, log *slog.Logger, loc *time.Location) *Scheduler {
	if loc == nil {
		loc = time.Local
	}
	return &Scheduler{
		db:  database,
		log: log,
		loc: loc,
		cr:  newCronEngine(log, loc),
	}
}

// RebuildWithLocation replaces the cron engine with one that fires in newLoc and
// re-registers every entry. robfig/cron fixes its location at construction, so a
// zone change cannot be applied in place — the engine is swapped wholesale. The
// call is idempotent (a no-op when newLoc already matches the active zone) and
// mutex-guarded against concurrent rebuilds. It is wired to the
// ScheduleTimezoneReload hook and fired after the operator saves a new zone.
func (s *Scheduler) RebuildWithLocation(ctx context.Context, newLoc *time.Location) error {
	if newLoc == nil {
		newLoc = time.Local
	}
	s.mu.Lock()
	if s.loc != nil && s.loc.String() == newLoc.String() {
		s.mu.Unlock()
		return nil // idempotent: zone unchanged
	}
	old := s.cr
	s.cr = newCronEngine(s.log, newLoc)
	s.loc = newLoc
	s.mu.Unlock()

	// Stop the old engine (lets in-flight fires finish) before re-registering and
	// starting the new one. Reload re-adds every entry from the DB into s.cr.
	if old != nil {
		old.Stop()
	}
	reloadErr := s.Reload(ctx)
	// ALWAYS start the new engine, even if the reload failed — mirroring Start()'s
	// non-fatal philosophy. The zone is already adopted (s.loc/s.cr swapped above);
	// a failed reload means the engine runs with no/partial entries until the next
	// backstop or callback reload re-populates it, rather than wedging scheduling
	// entirely (principle §2.2: the scheduler never wedges). A retry to the same
	// zone is then a safe no-op (idempotency check) because the backstop converges.
	s.cr.Start()
	if reloadErr != nil {
		s.log.Error("scheduler: timezone rebuild reload failed; engine started, entries reload via backstop", "zone", newLoc.String(), "err", reloadErr)
		return reloadErr
	}
	s.log.Info("scheduler: rebuilt for timezone change", "zone", newLoc.String())
	return nil
}

// SetWorkflowFirer installs the callback used to dispatch scheduled workflows.
// Call before Start (or before the first reload that registers workflows).
func (s *Scheduler) SetWorkflowFirer(f WorkflowFirer) {
	s.mu.Lock()
	s.wfFirer = f
	s.mu.Unlock()
}

// Start loads enabled scheduled jobs/workflows and starts the cron loop.
// It blocks until ctx is cancelled. A low-frequency backstop ticker reloads if
// the git SHA changed without an onSyncComplete callback reaching us.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.Reload(ctx); err != nil {
		s.log.Error("scheduler: initial load failed", "err", err)
		// Non-fatal: cron will still run, just with no entries until the next
		// reload (callback or backstop).
	}
	s.cr.Start()
	go s.backstopLoop(ctx)
	go s.pendingLoop(ctx) // AR — deferred ad-hoc runs; first pass is restart catch-up
	// RX — reactions. A separate loop from pendingLoop even though both end at
	// pending_runs: this one is a PRODUCER (it decides what to park) and that one
	// is the consumer (it decides what to fire). Its first pass anchors the
	// cursor rather than discharging history.
	go s.reactionLoop(ctx)
	// SL — the two retrospective scans. Neither fires anything: one warns that a
	// run is late, the other that one never started. Both anchor on their first
	// pass rather than discharging history (see sla.go).
	go s.slaLoop(ctx)
	go s.missedLoop(ctx)
	<-ctx.Done()
	stopCtx := s.cr.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(10 * time.Second):
		s.log.Warn("scheduler: cron stop timed out after 10s")
	}
	return nil
}

// backstopLoop periodically reloads if the git SHA advanced, as a fallback for a
// missed/failed onSyncComplete callback.
func (s *Scheduler) backstopLoop(ctx context.Context) {
	t := time.NewTicker(reloadBackstopInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ReloadIfChanged(ctx, s.currentSHA(ctx))
		}
	}
}

// currentSHA reads the last-synced git SHA from git_sync_state (empty if unknown).
func (s *Scheduler) currentSHA(ctx context.Context) string {
	var sha sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT last_sha FROM git_sync_state WHERE id = 1`).Scan(&sha)
	return sha.String
}

// ReloadIfChanged reloads only when sha differs from the SHA the current cron
// entries were loaded from, or if the database-level fingerprint has drifted.
// This is the onSyncComplete hook — it avoids tearing down and re-adding all
// entries on every no-op poll. An empty sha forces a reload.
func (s *Scheduler) ReloadIfChanged(ctx context.Context, sha string) {
	fp := s.dbFingerprint(ctx)

	s.mu.Lock()
	cur := s.loadedSHA
	curFP := s.loadedFingerprint
	s.mu.Unlock()

	if sha != "" && sha == cur && fp == curFP {
		return
	}
	if err := s.Reload(ctx); err != nil {
		s.log.Error("scheduler: reload-if-changed failed", "err", err)
	}
}

// Reload removes all existing entries and re-registers them from the DB.
// Safe to call multiple times (e.g. after a git sync rebuilds the definition
// tables). It reads jobs and workflows from definition_schedules.
func (s *Scheduler) Reload(ctx context.Context) error {
	// Remove all existing entries.
	for _, e := range s.cr.Entries() {
		s.cr.Remove(e.ID)
	}

	if err := s.reloadJobs(ctx); err != nil {
		return err
	}
	if err := s.reloadWorkflows(ctx); err != nil {
		return err
	}

	fp := s.dbFingerprint(ctx)

	s.mu.Lock()
	s.loadedSHA = s.currentSHA(ctx)
	s.loadedFingerprint = fp
	s.reloads++
	s.mu.Unlock()
	return nil
}

// dbFingerprint generates a simple string fingerprint of the jobs, workflows, and
// definition_schedules count and maximum modification timestamp. This is used by
// the backstop loop to detect DB-authored changes that bypassed onSyncComplete.
func (s *Scheduler) dbFingerprint(ctx context.Context) string {
	var jobCount, wfCount, schedCount int
	var maxJobTime, maxWfTime sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobCount)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflows`).Scan(&wfCount)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM definition_schedules`).Scan(&schedCount)
	_ = s.db.QueryRowContext(ctx, `SELECT MAX(COALESCE(last_modified_at, created_at, synced_at, '')) FROM jobs`).Scan(&maxJobTime)
	_ = s.db.QueryRowContext(ctx, `SELECT MAX(COALESCE(last_modified_at, created_at, synced_at, '')) FROM workflows`).Scan(&maxWfTime)
	return fmt.Sprintf("%d:%d:%d:%s:%s", jobCount, wfCount, schedCount, maxJobTime.String, maxWfTime.String)
}

func (s *Scheduler) reloadJobs(ctx context.Context) error {
	type jobRow struct {
		schedName, cron   string
		envJSON           sql.NullString
		startAt, endAt    sql.NullString
		interval          sql.NullString
		name, source      string
		runType           string
		scope             sql.NullString
		concurrencyPolicy string
		concurrencyKey    sql.NullString
		uid               string
	}

	// Source-aware join (A9): a (source,name) job binds only to its own
	// (owner_source,owner_name) schedules, so same-named git/amadeus jobs don't
	// cross-fire each other's schedules.
	// R2-3 — joined on the entry's owner_uid, with the (name, source) pair kept
	// as the arm for entries that carry no uid. The fallback is not optional
	// here: a schedule entry that drops out of THIS join simply stops firing,
	// with no error and nothing in the log, so a uid-only join would turn any
	// missed writer into silently unscheduled work. Same shape as the cascade
	// triggers (migration 1020), for the same reason.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ds.name, ds.cron, ds.env, ds.start_at, ds.end_at, ds.interval,
		       j.name, j.source, j.run_type, j.scope, j.concurrency_policy, j.concurrency_key,
		       COALESCE(j.uid,'')
		FROM definition_schedules ds
		JOIN jobs j ON (ds.owner_uid IS NOT NULL AND j.uid = ds.owner_uid)
		            OR (ds.owner_uid IS NULL AND j.name = ds.owner_name AND j.source = ds.owner_source)
		-- RH: a binned definition must stop firing. The soft delete is an UPDATE,
		-- so the AFTER DELETE cascade never ran and ds still has its rows — this
		-- filter is what actually retires the entry.
		WHERE ds.owner_kind = 'job' AND j.enabled = 1 AND j.deleted_at IS NULL
	`)
	if err != nil {
		return fmt.Errorf("query scheduled jobs: %w", err)
	}
	defer rows.Close()

	var jobs []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.schedName, &j.cron, &j.envJSON, &j.startAt, &j.endAt, &j.interval,
			&j.name, &j.source, &j.runType, &j.scope, &j.concurrencyPolicy, &j.concurrencyKey,
			&j.uid); err != nil {
			s.log.Error("scheduler: scan job schedule row", "err", err)
			continue
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate scheduled jobs: %w", err)
	}

	for _, j := range jobs {
		// One seam for all three modes (cron / interval / once): ParseSpec
		// resolves the mode and applies the activation window, so the engine
		// registers every entry identically.
		spec := cronutil.Spec{Cron: j.cron, Interval: j.interval.String, Window: parseWindowCols(j.startAt, j.endAt)}
		sched, err := cronutil.ParseSpec(spec)
		if err != nil {
			s.log.Error("scheduler: invalid schedule spec", "job", j.name, "schedule", j.schedName,
				"spec", cronutil.DescribeSpec(spec), "err", err)
			continue
		}
		scope := ""
		if j.scope.Valid {
			scope = j.scope.String
		}
		concKey := cronutil.ConcurrencyKey(j.concurrencyKey.String, j.uid, j.source, j.name)
		envJSON := j.envJSON.String
		s.cr.Schedule(sched, cron.FuncJob(func() {
			s.fire(j.source, j.name, j.uid, j.runType, scope, j.concurrencyPolicy, concKey, j.schedName, envJSON)
		}))
		s.log.Info("scheduler: registered job schedule", "job", j.name, "schedule", j.schedName,
			"spec", cronutil.DescribeSpec(spec), "window", windowLogState(spec.Window, time.Now()))
	}
	return nil
}

func (s *Scheduler) reloadWorkflows(ctx context.Context) error {
	type wfRow struct {
		schedName, cron string
		envJSON         sql.NullString
		startAt, endAt  sql.NullString
		interval        sql.NullString
		name, source    string
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ds.name, ds.cron, ds.env, ds.start_at, ds.end_at, ds.interval, w.name, w.source
		FROM definition_schedules ds
		-- R2-3 — uid-keyed with a name arm, exactly as reloadJobs above; see the
		-- comment there for why the fallback is mandatory rather than defensive.
		JOIN workflows w ON (ds.owner_uid IS NOT NULL AND w.uid = ds.owner_uid)
		                 OR (ds.owner_uid IS NULL AND w.name = ds.owner_name AND w.source = ds.owner_source)
		WHERE ds.owner_kind = 'workflow' AND w.enabled = 1 AND w.deleted_at IS NULL -- RH
	`)
	if err != nil {
		return fmt.Errorf("query scheduled workflows: %w", err)
	}
	defer rows.Close()

	var wfs []wfRow
	for rows.Next() {
		var wf wfRow
		if err := rows.Scan(&wf.schedName, &wf.cron, &wf.envJSON, &wf.startAt, &wf.endAt, &wf.interval, &wf.name, &wf.source); err != nil {
			s.log.Error("scheduler: scan workflow schedule row", "err", err)
			continue
		}
		wfs = append(wfs, wf)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate scheduled workflows: %w", err)
	}

	for _, wf := range wfs {
		spec := cronutil.Spec{Cron: wf.cron, Interval: wf.interval.String, Window: parseWindowCols(wf.startAt, wf.endAt)}
		sched, err := cronutil.ParseSpec(spec)
		if err != nil {
			s.log.Error("scheduler: invalid workflow schedule spec", "workflow", wf.name, "schedule", wf.schedName,
				"spec", cronutil.DescribeSpec(spec), "err", err)
			continue
		}
		envJSON := wf.envJSON.String
		s.cr.Schedule(sched, cron.FuncJob(func() {
			s.fireWorkflow(wf.source, wf.name, wf.schedName, envJSON)
		}))
		s.log.Info("scheduler: registered workflow schedule", "workflow", wf.name, "schedule", wf.schedName,
			"spec", cronutil.DescribeSpec(spec), "window", windowLogState(spec.Window, time.Now()))
	}
	return nil
}

// fire is called by the cron engine on each tick.  It honors operator pause,
// the global concurrency cap, and the S16 Forbid policy, then enqueues the run
// tagged with the schedule entry name and its env snapshot.
func (s *Scheduler) fire(source, jobName, jobUID, runType, scope, policy, concKey, scheduleName, envJSON string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Operator pause (migration 030; 170 made it source/owner_kind aware): a
	// paused job's cron fires are skipped.
	if s.isPaused(ctx, source, "job", jobName) {
		s.log.Info("scheduler: skip fire — job paused", "job", jobName, "source", source, "schedule", scheduleName)
		// SL-A: record it. A pause is an EXPECTED miss, and the missed-run
		// detector can only call a fire expected if the suppression left a row —
		// before this, pause and "the scheduler was dead" were indistinguishable.
		// ConcurrencyKey is deliberately left unset, as on the calendar path: PP-L8
		// assigns it only to Forbid runs, and dedupeEpisodeReason must not be
		// visible to the key-based Forbid de-dupe.
		if err := s.recordSuppression(ctx, EnqueueParams{
			JobName: jobName, JobSource: source, JobUID: jobUID, RunType: runType, Scope: scope,
			ScheduleName: scheduleName, Executor: ResolveExecutor(ctx, s.db, source, jobName, runType),
		}, skipRecord{Reason: reasonJobPaused, Mode: dedupeEpisodeReason}); err != nil {
			s.log.Error("scheduler: record paused skip", "job", jobName, "err", err)
		}
		return
	}

	// CAL-6 — working calendars. Sits immediately after the pause check and
	// BEFORE the concurrency cap on purpose: a fire suppressed by policy must be
	// recorded as suppressed-by-policy, not swallowed by a cap that happened to be
	// full at the same moment. Bindings are read fresh per fire (calendar_gate.go).
	if g := s.calendarVerdict(ctx, source, "job", jobName, scheduleName); g.Suppressed() {
		v := g.Verdict
		s.log.Info("scheduler: skip fire — suppressed by calendar",
			"job", jobName, "source", source, "schedule", scheduleName, "day", g.Day,
			"calendar", v.Calendar, "polarity", v.Polarity, "label", v.Label, "recorded", v.Record)
		// CAL-Q4: skip-polarity suppressions always record — that is the compliance
		// question. Only-polarity ones record solely when the calendar opted in,
		// or a weekdays-only entry would write ten audit rows every weekend.
		if v.Record {
			// ConcurrencyKey is deliberately left unset: PP-L8 assigns it only to
			// Forbid-policy runs, and the calendar de-dupe is keyed on the day
			// rather than the key precisely because most jobs carry neither.
			if err := s.recordSuppression(ctx, EnqueueParams{
				JobName: jobName, JobSource: source, JobUID: jobUID, RunType: runType, Scope: scope,
				ScheduleName: scheduleName, Executor: ResolveExecutor(ctx, s.db, source, jobName, runType),
			}, g.record()); err != nil {
				s.log.Error("scheduler: record calendar suppression", "job", jobName, "err", err)
			}
		}
		return
	}

	// Check global concurrency cap from settings (default 5).
	maxConcurrent := s.maxConcurrent(ctx)
	active, err := s.activeRunCount(ctx)
	if err != nil {
		s.log.Error("scheduler: check active runs", "err", err)
		return
	}
	if active >= maxConcurrent {
		s.log.Info("scheduler: skip fire — global concurrency cap reached",
			"job", jobName, "active", active, "cap", maxConcurrent)
		// SL-A: same argument as the pause skip above. The cap is transient, so
		// the episode rule re-records once a real run lands in between — a fleet
		// at cap for an hour leaves one row, but two separate cap episodes on the
		// same day leave two.
		if err := s.recordSuppression(ctx, EnqueueParams{
			JobName: jobName, JobSource: source, JobUID: jobUID, RunType: runType, Scope: scope,
			ScheduleName: scheduleName, Executor: ResolveExecutor(ctx, s.db, source, jobName, runType),
		}, skipRecord{Reason: reasonConcurrencyCap, Mode: dedupeEpisodeReason}); err != nil {
			s.log.Error("scheduler: record capped skip", "job", jobName, "err", err)
		}
		return
	}

	// Job-level env (JC10/JC11): the BASE layer beneath the firing schedule's env
	// (schedule wins on key collision, Q-JC9). Read fresh per fire — like
	// ResolveExecutor below — so an edit takes effect without waiting for the next
	// scheduler reload. NULL/empty job-env ⇒ MergeJSON returns the schedule env
	// unchanged (R2 no-op).
	//
	// TG-1: target_host rides the SAME per-fire read. The definition's single-host
	// pin is meaningless unless every trigger path delivers it onto the run — before
	// this, only the manual handler did, so a pinned job's cron fires resolved
	// targetHost="" and fanned out across the WHOLE scope (execspec.ResolveTargets).
	// Freshness semantics match env exactly: editing the pin applies on the next
	// fire, with no scheduler reload.
	// CA-10: the job-spec "connect as" identity rides the same per-fire read,
	// with the same freshness semantics — folded onto the run only for
	// identity-capable run types (RP-7; terraform authenticates through its
	// providers, and the stored field is advisory-warned at sync).
	// R2-5 — the per-fire read keys on the identity the reload resolved, never
	// on the (name, source) pair a sibling may now share.
	var jobEnv, jobTargetHost, jobSSHUser, jobSSHCred sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT env_json, target_host, ssh_user, ssh_credential FROM jobs
		  WHERE CASE WHEN ? != '' THEN uid = ? ELSE name = ? AND source = ? END`,
		jobUID, jobUID, jobName, source).
		Scan(&jobEnv, &jobTargetHost, &jobSSHUser, &jobSSHCred)
	if !execspec.IdentityCapableRunType(runType) {
		jobSSHUser.String, jobSSHCred.String = "", ""
	}

	// Shared params: both the Forbid skip-record path and the enqueue success
	// path below mirror the same field assembly.
	// PP-L8: only GATE-HOLDING policies set ConcurrencyKey, so the partial unique
	// index (which excludes NULL) fires for them and not for Allow runs.
	//
	// QP widened this from "Forbid" to HoldsGate: a Queue run must carry the key
	// too, or the next fire has nothing to queue behind and the policy silently
	// degrades to Allow. The unique index is then what actually enforces
	// one-at-a-time; Forbid and Queue differ only in what happens to the fire
	// that meets it.
	effectiveConcKey := ""
	if cronutil.HoldsGate(policy) {
		effectiveConcKey = concKey
	}
	// M3/T3.6 — snapshot the agency SET the effective scope belongs to onto the run,
	// frozen at enqueue and intersected against runner membership at claim time
	// (hard isolation). The scalar ScopeAgency is gone: a scope may now belong to
	// several agencies, so there is no single answer to snapshot.
	scopeAgencies, _ := execspec.ScopeAgencies(ctx, s.db, scope)
	// M4 — advisory: a scheduled run whose agencies have no online runner will sit
	// queued until one comes online. Log it (no human is watching an automated fire).
	if ok, _ := execspec.AgenciesHaveOnlineRunner(ctx, s.db, scopeAgencies); !ok && len(scopeAgencies) > 0 {
		s.log.Warn("scheduler: no online runner in the run's agencies — run will wait",
			"job", jobName, "agencies", scopeAgencies)
	}

	// RA-24 — the SCHEDULED half, and the one that actually matters under the ops
	// model (§13): admins wire jobs and SCHEDULE them; the ad-hoc trigger is the
	// exception. An unbound fire of a credential-consuming job resolves nothing and
	// is claimable by no departmental runner, with NO HUMAN WATCHING — so unlike the
	// trigger, there is nobody to hand a 422 to.
	//
	// Recorded as a terminal skipped run rather than dropped with a log line, for
	// exactly the reason recordSkippedFire exists: a fire that vanishes into the log
	// is a fire nobody knows didn't happen. The operator sees it in History with the
	// reason attached, next to the runs that did work.
	// R2F-1 — by identity, like every other per-fire read above: the script whose
	// bindings this probe checks must be the one THIS job references, not a
	// same-named sibling's.
	var scriptRef sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT script_ref FROM jobs WHERE CASE WHEN ? != '' THEN uid = ? ELSE name = ? AND source = ? END`,
		jobUID, jobUID, jobName, source).Scan(&scriptRef)
	owners := runref.RunOwners(source, jobName, jobUID, scriptRef.String)
	executor := ResolveExecutor(ctx, s.db, source, jobName, runType)
	blocked, berr := runref.UnboundRunBlocked(ctx, s.db, owners, scope, scopeAgencies)
	if berr != nil {
		s.log.Error("scheduler: check unbound references", "job", jobName, "err", berr)
		return
	}
	if len(blocked) > 0 {
		s.log.Warn("scheduler: skip fire — unbound run consumes department-owned credentials",
			"job", jobName, "reference", blocked[0].Reference, "detail", runref.UnboundRefusal(blocked))
		if err := s.recordSuppression(ctx, EnqueueParams{
			JobName: jobName, JobSource: source, JobUID: jobUID, RunType: runType, Scope: scope,
			TargetHost: jobTargetHost.String, ScheduleName: scheduleName,
			ConcurrencyKey: effectiveConcKey, Executor: executor,
		}, skipRecord{Reason: runref.QueuedReasonUnboundReferences, Mode: dedupeEpisode}); err != nil {
			s.log.Error("scheduler: record unbound-reference skip", "job", jobName, "err", err)
		}
		return
	}
	// KB — a key-bound job whose fire resolves to the ssh executor: the executor
	// cannot deliver the key, so the fire is recorded as skipped (same shape as
	// the unbound refusal above, same argument — nobody is watching a cron fire).
	keys, kerr := runref.KeyBindingsOnSSH(ctx, s.db, owners, executor)
	if kerr != nil {
		s.log.Error("scheduler: check key bindings", "job", jobName, "err", kerr)
		return
	}
	if len(keys) > 0 {
		s.log.Warn("scheduler: skip fire — key-bound job resolved to the ssh executor",
			"job", jobName, "reference", keys[0].Reference, "detail", runref.KeyBindingRefusal(keys))
		if err := s.recordSuppression(ctx, EnqueueParams{
			JobName: jobName, JobSource: source, JobUID: jobUID, RunType: runType, Scope: scope,
			TargetHost: jobTargetHost.String, ScheduleName: scheduleName,
			ConcurrencyKey: effectiveConcKey, Executor: executor,
		}, skipRecord{Reason: runref.ReasonKeyBindingOnSSH, Mode: dedupeEpisodeReason}); err != nil {
			s.log.Error("scheduler: record key-binding skip", "job", jobName, "err", err)
		}
		return
	}
	params := EnqueueParams{
		JobName:        jobName,
		JobSource:      source,
		RunType:        runType,
		Scope:          scope,
		TargetHost:     jobTargetHost.String,
		TriggerKind:    "scheduled",
		TriggeredBy:    "scheduler",
		ConcurrencyKey: effectiveConcKey,
		ScheduleName:   scheduleName,
		EnvJSON:        envmerge.MergeJSON(jobEnv.String, envJSON),
		Executor:       executor,
		SSHUser:        jobSSHUser.String,
		SSHCredential:  jobSSHCred.String,
		AgenciesJSON:   execspec.MarshalAgencies(scopeAgencies),
	}

	// S16 gate check. Forbid loses the fire; Queue parks it (QP).
	if cronutil.HoldsGate(policy) {
		conflict, err := s.hasActiveRunForKey(ctx, concKey)
		if err != nil {
			s.log.Error("scheduler: concurrency gate check", "err", err)
			return
		}
		if conflict {
			if policy == cronutil.PolicyQueue {
				queued, qerr := TryQueue(ctx, s.db, params)
				if qerr != nil {
					s.log.Error("scheduler: queue fire", "job", jobName, "err", qerr)
					return
				}
				if queued {
					s.log.Info("scheduler: queued fire behind an active run",
						"job", jobName, "concurrency_key", concKey, "depth", QueueDepth(ctx, s.db, concKey))
					return
				}
				// Cap reached — fall back to Forbid's behaviour and SAY so. An
				// unbounded queue behind a wedged job is the runaway this cap
				// exists to prevent (the reaction-brake instinct), but a refusal
				// that leaves no trace is the invisible failure this file keeps
				// having to fix.
				// dedupeEpisodeReason, not dedupeEpisode, and the key is dropped
				// for the same reason the pause and cap paths drop it: keyed
				// de-dupe cannot tell a queue-full refusal from a Forbid skip, so
				// whichever landed first would swallow the other — and two jobs
				// sharing a custom concurrency key would refuse in silence. The
				// reason text is load-bearing here (see QueueFullReason).
				capParams := params
				capParams.ConcurrencyKey = ""
				if err := s.recordSuppression(ctx, capParams, skipRecord{
					Reason: QueueFullReason,
					Mode:   dedupeEpisodeReason,
				}); err != nil {
					s.log.Error("scheduler: record queue-full skip", "job", jobName, "err", err)
				}
				s.log.Warn("scheduler: skip fire — concurrency queue is full",
					"job", jobName, "concurrency_key", concKey, "cap", QueueCap)
				return
			}
			// V1.1-12: surface the suppressed fire in History as a terminal
			// 'skipped' run (best-effort; de-duped per blocking episode).
			if err := s.recordSuppression(ctx, params, skipRecord{
				Reason: "Skipped: a run for this job is already active (Forbid policy)",
				Mode:   dedupeEpisode,
			}); err != nil {
				s.log.Error("scheduler: record skipped fire", "job", jobName, "err", err)
			}
			s.log.Info("scheduler: skip fire — Forbid concurrency conflict",
				"job", jobName, "concurrency_key", concKey)
			return
		}
	}

	if err := EnqueueRun(ctx, s.db, params); err != nil {
		// PP-L8: the partial unique index on runs.concurrency_key catches any
		// check-then-insert race that slipped past the explicit Forbid check above
		// (two concurrent fires both reading no active run, then racing to insert).
		// Treat the constraint violation exactly like a Forbid conflict: record a
		// skipped run and log, rather than surfacing it as an unexpected error.
		if IsForbidConflict(err) {
			if rerr := s.recordSuppression(ctx, params, skipRecord{
				Reason: "Skipped: concurrent fire race resolved by DB constraint (Forbid policy)",
				Mode:   dedupeEpisode,
			}); rerr != nil {
				s.log.Error("scheduler: record race-resolved skip", "job", jobName, "err", rerr)
			}
			s.log.Info("scheduler: skip fire — Forbid race resolved by constraint",
				"job", jobName, "concurrency_key", concKey)
			return
		}
		s.log.Error("scheduler: enqueue run", "job", jobName, "err", err)
	}
}

// ResolveExecutor picks the executor for an automatically-enqueued run (R5.1)
// with the precedence: job spec.executor > global execution.defaultExecutor >
// run-type capability default (shell ⇒ ssh, ansible/terraform ⇒ runner).
// Scheduled/workflow runs have no per-trigger override; the manual-trigger path
// layers that override on top of this same precedence (api.resolveExecutor).
func ResolveExecutor(ctx context.Context, database *sql.DB, jobSource, jobName, runType string) string {
	if jobSource == "" {
		jobSource = "git"
	}
	var jobExec sql.NullString
	_ = database.QueryRowContext(ctx, `SELECT executor FROM jobs WHERE name = ? AND source = ?`, jobName, jobSource).Scan(&jobExec)
	if jobExec.Valid && (jobExec.String == "ssh" || jobExec.String == "runner") {
		return jobExec.String
	}
	var def sql.NullString
	_ = database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'defaultExecutor'`).Scan(&def)
	if def.Valid && (def.String == "ssh" || def.String == "runner") {
		// A global ssh default cannot run ansible/terraform — fall through to the
		// capability default for those rather than enqueue an unclaimable run.
		if !(def.String == "ssh" && !execspec.SupportedRunType(runType)) {
			return def.String
		}
	}
	if execspec.SupportedRunType(runType) {
		return "ssh"
	}
	return "runner"
}

// fireWorkflow dispatches a scheduled workflow via the injected firer, honoring
// the workflow pause sentinel ("__wf__"+name) and the global concurrency cap.
func (s *Scheduler) fireWorkflow(source, workflowName, scheduleName, envJSON string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.mu.Lock()
	firer := s.wfFirer
	s.mu.Unlock()
	if firer == nil {
		s.log.Warn("scheduler: no workflow firer set — skipping", "workflow", workflowName)
		return
	}

	if s.isPaused(ctx, source, "workflow", workflowName) {
		s.log.Info("scheduler: skip fire — workflow paused", "workflow", workflowName, "source", source, "schedule", scheduleName)
		// SL-A — the workflow half. CAL-32 already argued why this cannot reuse
		// recordSkippedFire: that one is job-shaped throughout.
		if err := s.recordWorkflowSuppression(ctx, source, workflowName, scheduleName,
			skipRecord{Reason: reasonWorkflowPaused, Mode: dedupeEpisodeReason}); err != nil {
			s.log.Error("scheduler: record paused skip", "workflow", workflowName, "err", err)
		}
		return
	}

	// CAL-6 — the workflow half of the calendar gate. Suppressing the workflow's
	// own schedule entry suppresses the WHOLE run; the step jobs' calendars are
	// deliberately not consulted (§2.6), because a per-step calendar would mean a
	// workflow that half-runs, which is worse than either outcome.
	if g := s.calendarVerdict(ctx, source, "workflow", workflowName, scheduleName); g.Suppressed() {
		v := g.Verdict
		s.log.Info("scheduler: skip fire — suppressed by calendar",
			"workflow", workflowName, "source", source, "schedule", scheduleName, "day", g.Day,
			"calendar", v.Calendar, "polarity", v.Polarity, "label", v.Label, "recorded", v.Record)
		if v.Record {
			if err := s.recordWorkflowSuppression(ctx, source, workflowName, scheduleName, g.record()); err != nil {
				s.log.Error("scheduler: record calendar suppression", "workflow", workflowName, "err", err)
			}
		}
		return
	}

	maxConcurrent := s.maxConcurrent(ctx)
	active, err := s.activeRunCount(ctx)
	if err != nil {
		s.log.Error("scheduler: check active runs", "err", err)
		return
	}
	if active >= maxConcurrent {
		s.log.Info("scheduler: skip fire — global concurrency cap reached",
			"workflow", workflowName, "active", active, "cap", maxConcurrent)
		// SL-A. CAL-32's header noted fireWorkflow "dropped even its
		// concurrency-cap skips with a log line" — this is that gap closed.
		if err := s.recordWorkflowSuppression(ctx, source, workflowName, scheduleName,
			skipRecord{Reason: reasonConcurrencyCap, Mode: dedupeEpisodeReason}); err != nil {
			s.log.Error("scheduler: record capped skip", "workflow", workflowName, "err", err)
		}
		return
	}

	firer(ctx, source, workflowName, scheduleName, envJSON)
}

// isPaused reports whether a paused_jobs row exists for a definition, keyed by
// (source, owner_kind, name) since migration 170 (Q-G retired the "__wf__" name
// prefix in favor of an explicit owner_kind column).
func (s *Scheduler) isPaused(ctx context.Context, source, ownerKind, name string) bool {
	return IsPaused(ctx, s.db, source, ownerKind, name)
}

// IsPaused is the DB-level twin of (*Scheduler).isPaused, exported for trigger
// paths that live outside this package (ET-D's file-arrival watcher).
//
// Nothing downstream of Enqueue consults paused_jobs — the pause is checked by
// each trigger path, one gate per producer (see the reaction.go note). A new
// producer that skips this check fires paused jobs.
func IsPaused(ctx context.Context, database *sql.DB, source, ownerKind, name string) bool {
	var n int
	_ = database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM paused_jobs WHERE source = ? AND owner_kind = ? AND name = ?`,
		source, ownerKind, name).Scan(&n)
	return n > 0
}

// AtCapacity reports whether the fleet-wide concurrency cap is reached
// (queued+running vs the operator's max). Exported for the same reason as
// IsPaused: the cap is a fire-time gate with no claim-time backstop, so every
// producer applies it or its runs escape the cap.
func AtCapacity(ctx context.Context, database *sql.DB) bool {
	var active int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE status IN ('queued','running')`).Scan(&active); err != nil {
		return false
	}
	return active >= maxConcurrentDB(ctx, database)
}

// EnqueueParams holds the fields for an enqueued run.
type EnqueueParams struct {
	JobName   string
	JobSource string // git | amadeus (A9); empty ⇒ 'git'. Snapshotted on the run + used to resolve the job's denormalized fields by (name,source).
	// JobUID is the job's permanent identity (R2-5). Producers that resolved a
	// real job row pass it, and every denormalizing subselect in the run INSERT
	// keys on it — the only key that stays unambiguous now that a (source,
	// name) pair may match two jobs. Empty falls back to the pair, which the
	// enqueue resolves ONCE up front rather than per-subselect.
	JobUID         string
	RunType        string
	Scope          string
	TargetHost     string // EX.3/EX.5 — single-host target (empty ⇒ scope fan-out)
	TriggerKind    string // manual | scheduled | workflow | webhook
	TriggeredBy    string // email or "scheduler"
	ConcurrencyKey string
	WorkflowRunID  string // FK to workflow_runs.id; empty if standalone
	Executor       string // EX.3 — runner | ssh; empty ⇒ runs-table default ('ssh')
	ScheduleName   string // which schedule entry fired (empty ⇒ NULL)
	EnvJSON        string // env snapshot for this run as a JSON string (empty ⇒ NULL)
	OverrideJSON   string // F3 — ad-hoc override envelope (the env/hosts/scope/executor an operator supplied at trigger time) as JSON; empty ⇒ NULL. Set only on the manual run path; sibling to EnvJSON.
	// SSHUser / SSHCredential (CA, the ssh-user plan) — the run's frozen
	// effective "connect as" identity: remote login and ssh_credentials LABEL
	// (names only, CA-Q2). Phase A sets them from the manual trigger's per-run
	// override; Phase B folds a job-spec default in at every producer. Empty ⇒
	// NULL ⇒ the pre-CA per-host resolution.
	SSHUser       string
	SSHCredential string
	// AgenciesJSON is the run's frozen agency SET snapshot (migration 680, AG-Q2b) —
	// the JSON array claimRun intersects against runner membership. It REPLACES the
	// scalar Agency field, which was dual-written through v0.52.2 and dropped with
	// runs.agency in migration 700. Empty ⇒ the column default '[]'; callers use
	// execspec.MarshalAgencies so the value is always a well-formed array — malformed
	// JSON here would make json_each throw inside the claim query.
	AgenciesJSON string
	// ScheduledFor (FX-B1) is the instant a SCHEDULE expected this run, carried
	// across a Queue-policy park so the missed-run detector can recognise a
	// late-promoted run as the fire it was. Empty ⇒ NULL ⇒ this run did not
	// originate as a parked scheduled fire, which is true of every immediate
	// path. Only promoteOne sets it.
	ScheduledFor string `json:",omitempty"`
	// RunnerTag (RT-2) is the runner pin frozen onto the run — the value claimRun
	// matches against runner_tags (mig. 1070/1080).
	//
	// A *string carrying the trigger-level tri-state (RT-Q6/RT-G8):
	//
	//	nil  → this producer has no opinion; the enqueue resolves the job's two
	//	       layers itself (override, else declared) in SQL
	//	""   → this run is explicitly UNPINNED, even if the job is pinned
	//	"x"  → this run is pinned to x
	//
	// nil is the default, so every producer that predates RT-2 — scheduled fires,
	// pending-run promotion, workflow steps, reactions — inherits the job's pin
	// correctly without being touched. Only the manual trigger, which has a
	// per-run override rung the others lack, passes a non-nil value. A plain
	// string here would collapse nil and "" and make an operator's deliberate
	// per-run unpin silently re-inherit the job's pin.
	RunnerTag *string
	// Priority reorders the runner claim: higher goes first, ties break oldest-
	// first (QP). Per-TRIGGER only (PF-Q11) — there is deliberately no job-spec
	// default, because a standing priority is how one job starves another
	// forever rather than merely going first today.
	Priority int
	// Policy is the job's concurrency policy, carried ONLY so a deferred
	// (pending) run can re-judge Forbid at promotion time without a jobs-table
	// join. The immediate paths judge Forbid before calling enqueue and leave
	// this empty; enqueue itself never reads it.
	Policy string `json:",omitempty"`
	// ReactionDepth / ReactedToRunID (RX) — the reaction chain's position and
	// the upstream run that caused this one.
	//
	// ReactionDepth is the runaway backstop: each hop inherits upstream+1 and the
	// reactor refuses at the ceiling. ReactedToRunID is the DURABLE provenance
	// link History renders in both directions; it lives on the run row because
	// the only other record of it is the retention-pruned delivery log, and a
	// because-of link that evaporates on retention is not a trail.
	//
	// Both are zero for every non-reaction path, which is every path today.
	ReactionDepth  int    `json:",omitempty"`
	ReactedToRunID string `json:",omitempty"`
}

// agenciesJSONOrDefault keeps the snapshot well-formed even if a caller forgets to
// set it: an empty string would violate the NOT NULL column and, worse, an empty
// STRING is not an empty ARRAY to json_each.
func (p EnqueueParams) agenciesJSONOrDefault() string {
	if p.AgenciesJSON == "" {
		return "[]"
	}
	return p.AgenciesJSON
}

func (p EnqueueParams) executorOrDefault() string {
	if p.Executor == "" {
		return "ssh"
	}
	return p.Executor
}

func (p EnqueueParams) jobSourceOrDefault() string {
	if p.JobSource == "" {
		return "git"
	}
	return p.JobSource
}

// resolveEnqueueUID pins the job identity for an enqueue. The pair fallback is
// single-row; if the pair is ambiguous (two amadeus siblings) the produced uid
// is whichever row SQLite returns — which is why every real producer passes
// p.JobUID and this fallback exists for tests and legacy callers only.
func resolveEnqueueUID(ctx context.Context, database *sql.DB, p EnqueueParams) string {
	if p.JobUID != "" {
		return p.JobUID
	}
	var uid sql.NullString
	_ = database.QueryRowContext(ctx,
		`SELECT uid FROM jobs WHERE name = ? AND source = ?`,
		p.JobName, p.jobSourceOrDefault()).Scan(&uid)
	return uid.String
}

// runnerTagExplicit reports whether this producer set a pin at all (RT-2): 1 when
// it did — including deliberately setting the empty string — and 0 when the
// enqueue should resolve the job's own layers instead.
func (p EnqueueParams) runnerTagExplicit() int {
	if p.RunnerTag != nil {
		return 1
	}
	return 0
}

// runnerTagValue is the bound value for the explicit arm. Meaningless (and
// ignored by the CASE) when runnerTagExplicit is 0.
func (p EnqueueParams) runnerTagValue() string {
	if p.RunnerTag != nil {
		return *p.RunnerTag
	}
	return ""
}

// EnqueueRun inserts a runs row with status='queued'.  This is the only write
// path into the runs table from B5; B4 updates status to running/terminal.
func EnqueueRun(ctx context.Context, database *sql.DB, p EnqueueParams) error {
	// RR-2: one body. This was a second copy of EnqueueRunWithID's INSERT that
	// discarded the trace id, and the two drifted (RR-0a, RR-0d). The name stays
	// for its caller and tests; the row it writes is now the same by construction.
	_, err := EnqueueRunWithID(ctx, database, p)
	return err
}

// skipDedupe selects how a suppressed fire collapses against the ones before it.
//
// CAL-8 made this explicit rather than leaving one hard-coded rule, because the
// original rule was stated in terms of THE LAST RUN'S STATUS rather than the
// episode it belonged to — so every later reason for skipping silently inherited
// a de-dupe policy written for Forbid. Adding a reason must now mean choosing its
// de-dupe rule.
type skipDedupe int

const (
	// dedupeEpisode collapses a storm of suppressions into one record per
	// blocking episode: a no-op while the most recent non-calendar run for this
	// concurrency key is already skipped. Correct for Forbid, where the same
	// blocker suppresses every fire until it clears.
	dedupeEpisode skipDedupe = iota
	// dedupeDay collapses to one record per suppressed DAY per schedule entry.
	// Correct for calendar suppression, where the "episode" rule would record a
	// three-day holiday weekend as a single skip — and where an unrelated Forbid
	// skip landing in between would swallow the calendar skip entirely.
	dedupeDay
	// dedupeEpisodeReason is dedupeEpisode for suppressions that carry NO
	// concurrency key: a no-op while the most recent non-calendar outcome for
	// this (job/workflow, schedule entry) is already a skip carrying THIS SAME
	// reason. Correct for pause and the global cap (SL-A).
	//
	// dedupeEpisode cannot serve them. It keys on concurrency_key, which PP-L8
	// assigns only to Forbid-policy runs — every Allow job carries NULL, and
	// `concurrency_key = NULL` matches nothing in SQL, so a paused `* * * * *`
	// entry would write ~1,440 audit rows a day. Matching on the reason instead
	// keeps the CAL-8 independence rule intact in both directions: a pause skip
	// can never stand in for a cap skip, and neither can be mistaken for a
	// Forbid skip, whose query stays key-based and therefore cannot see these
	// key-less rows at all.
	dedupeEpisodeReason
)

// Reasons for the suppressions recorded under dedupeEpisodeReason.
//
// Unlike every other skipRecord.Reason these strings are LOAD-BEARING: the
// de-dupe compares them, so editing one starts a fresh episode bucket (at worst
// one extra audit row) and two reasons sharing a string would collapse into each
// other. They are constants for exactly that reason — copy-edit deliberately.
const (
	reasonJobPaused      = "Skipped: the job is paused"
	reasonWorkflowPaused = "Skipped: the workflow is paused"
	reasonConcurrencyCap = "Skipped: the global concurrency cap was reached"
)

// skipRecord is everything a suppressed fire needs to leave an audit row.
type skipRecord struct {
	// Reason is the human-readable queued_reason. Free to be copy-edited under
	// dedupeEpisode and dedupeDay; no query depends on its text there.
	// EXCEPTION: under dedupeEpisodeReason the de-dupe compares it, so the text
	// is load-bearing — use the reason* constants above, don't inline a string.
	Reason string
	// Mode is this reason's de-dupe rule. Required — see skipDedupe.
	Mode skipDedupe
	// Calendar is the suppressing calendar's name, written to
	// suppressed_by_calendar. Empty for non-calendar skips, which is what the
	// dedupeEpisode query filters on to stay independent of this path.
	Calendar string
	// Day is the app-zone day the fire landed on ('YYYY-MM-DD'), used by
	// dedupeDay. Empty disables day de-duping (every suppression records), which
	// is the correct degradation for callers that do not use dedupeDay.
	Day string
	// Loc is the zone Day was rendered in, so stored UTC timestamps can be
	// re-rendered the same way when comparing. Nil degrades to time.Local, never
	// to UTC — a day comparison in UTC is the bug this feature exists to avoid.
	Loc *time.Location
	// At overrides the recorded timestamp (RFC3339 UTC); empty means now.
	//
	// Only the missed-run detector sets it, and it must: a miss is DISCOVERED
	// minutes after the fire it describes, and stamping discovery time would put
	// the row somewhere History does not match the gap it explains.
	At string
}

// recordSkippedFire inserts a terminal skipped run so a cron fire suppressed by a
// Forbid concurrency conflict (V1.1-12) or a working calendar (CAL-6) is visible
// in History instead of vanishing with only a log line.
//
// A skipped run never executes, so this INSERT intentionally omits env_json — the
// merged job/schedule env on p.EnvJSON (JC11) is not persisted onto skipped rows;
// the column stays NULL, exactly as before the job-level-env feature.
// recordSuppression is the ONE place a suppression becomes both a durable row
// and a notification (FX-D4, FX-Q7).
//
// `skipped` has been a valid, selectable notification trigger since the feature
// shipped — the API validates it, the UI offers it — and nothing could ever emit
// it: the only producers of a notify event were the two executor paths, which by
// definition run only when a run actually ran. An operator could author a rule
// on "skipped", see it listed, and never hear from it again. Silence is the one
// failure mode a notification system cannot afford, because a rule that never
// fires looks exactly like a system that never suppressed anything.
//
// Two deliberate exclusions:
//
//   - A DE-DUPED record emits nothing. recordSkippedFire collapses repeats per
//     day or per episode precisely so a per-minute schedule suppressed on a
//     holiday writes one row and not 1,440; emitting per attempt would undo that
//     in the one medium where volume is most expensive.
//   - A "Missed:" row emits nothing HERE. Those already notify through
//     TriggerMissedRun, which is a different and louder question ("a fire we
//     expected never happened") than the one this trigger answers ("a fire we
//     expected was deliberately stopped"). Emitting both would page twice for
//     one event.
func (s *Scheduler) recordSuppression(ctx context.Context, p EnqueueParams, rec skipRecord) error {
	wrote, err := recordSkippedFire(ctx, s.db, p, rec)
	if err != nil || !wrote {
		return err
	}
	s.notifySuppressed(ctx, "job", p.jobSourceOrDefault(), p.JobName, p.Scope, rec.Reason, rec.Mode)
	return nil
}

// recordWorkflowSuppression is recordSuppression's twin. Written at the same
// time deliberately: a guard added to one of these and not the other is how the
// last several defects in this area survived review.
func (s *Scheduler) recordWorkflowSuppression(ctx context.Context, source, workflowName, scheduleName string, rec skipRecord) error {
	wrote, err := recordSkippedWorkflowFire(ctx, s.db, source, workflowName, scheduleName, rec)
	if err != nil || !wrote {
		return err
	}
	s.notifySuppressed(ctx, "workflow", source, workflowName, "", rec.Reason, rec.Mode)
	return nil
}

func (s *Scheduler) notifySuppressed(ctx context.Context, ownerKind, source, name, scope, reason string, mode skipDedupe) {
	_ = ctx
	_ = source
	// The exact constant, not a prefix. reasonMissedFire is not in the
	// load-bearing-strings list at the top of this file, so it reads as free
	// text — and a copy-edit that broke a "Missed:" prefix match would double-page
	// every missed fire (once here, once through TriggerMissedRun) with nothing
	// failing.
	if reason == reasonMissedFire {
		return
	}
	// VOLUME. dedupeEpisode is the Forbid path, and its episode ENDS as soon as a
	// real run lands — so a per-minute Forbid job whose runs take 90s alternates
	// run, skip, run, skip and would emit something like 720 notifications a day
	// for one job. The rows are wanted (History must show every refusal); the
	// pages are not. Only the bounded modes notify: dedupeDay collapses to one per
	// job per day, and dedupeEpisodeReason to one per reason per episode.
	if mode == dedupeEpisode {
		return
	}
	n := s.notifierRef()
	if n == nil {
		return
	}
	n.RunEnded(notify.RunEvent{
		JobName:   name,
		Scope:     scope,
		OwnerKind: ownerKind,
		Status:    "skipped",
		Reason:    reason,
	})
}

func recordSkippedFire(ctx context.Context, database *sql.DB, p EnqueueParams, rec skipRecord) (bool, error) {
	dupe, err := skipAlreadyRecorded(ctx, database, p, rec)
	if err != nil {
		return false, err
	}
	if dupe {
		return false, nil
	}
	// A skipped fire carries WHAT was suppressed, not HOW it would have run: no
	// env, no override, no connect-as identity, no agencies, no priority. Built
	// field by field rather than from p wholesale so that stays a decision and
	// not an accident of a shorter column list (RR-2).
	_, err = InsertRun(ctx, database, RunRow{
		EnqueueParams: EnqueueParams{
			JobName:        p.JobName,
			JobSource:      p.JobSource,
			JobUID:         p.JobUID,
			RunType:        p.RunType,
			Scope:          p.Scope,
			TargetHost:     p.TargetHost,
			TriggerKind:    "scheduled",
			TriggeredBy:    "scheduler",
			ConcurrencyKey: p.ConcurrencyKey,
			Executor:       p.Executor,
			ScheduleName:   p.ScheduleName,
		},
		Status:               "skipped",
		QueuedReason:         rec.Reason,
		Terminal:             true,
		SuppressedByCalendar: rec.Calendar,
		CreatedAt:            rec.At,
	})
	if err != nil {
		return false, fmt.Errorf("insert skipped run: %w", err)
	}
	return true, nil
}

// skipAlreadyRecorded applies the record's de-dupe rule.
//
// The two modes are deliberately INDEPENDENT of each other — each filters to its
// own kind of skip — so a Forbid suppression landing between two calendar
// suppressions swallows neither, and vice versa. That independence is the actual
// fix behind CAL-8; before it, "the most recent run is already skipped" made
// every reason inherit whichever one happened to fire last.
func skipAlreadyRecorded(ctx context.Context, database *sql.DB, p EnqueueParams, rec skipRecord) (bool, error) {
	switch rec.Mode {
	case dedupeDay:
		// One record per (job, schedule entry, app-zone day). NOT keyed on
		// concurrency_key: PP-L8 sets that only for Forbid-policy runs, so every
		// Allow/Replace job carries NULL and a key-based rule would never collapse
		// anything — a `* * * * *` entry suppressed on a holiday would write ~1,440
		// audit rows for one suppressed day.
		//
		// The stored day is derived from created_at rather than kept in its own
		// column: created_at is written in UTC, so it is re-rendered here in the
		// SAME zone the verdict used. (A timezone change between two fires on the
		// same day can therefore let a second row through. That is a boundary
		// case of an operator action, and an extra audit row is the harmless side
		// to err toward.)
		day := rec.Day
		if day == "" {
			return false, nil // no day to compare against; record it
		}
		// LIMIT 50 is sufficient rather than arbitrary: this rule guarantees at
		// most ONE calendar-skip row per (job, entry, day), so today's row — if it
		// exists — is always among the newest few. The bound only matters if a
		// clock rollback leaves dozens of future-dated rows ahead of today's.
		rows, err := database.QueryContext(ctx, `
			SELECT created_at FROM runs
			 WHERE job_name = ? AND job_source = ? AND COALESCE(schedule_name,'') = ?
			   AND status = 'skipped' AND suppressed_by_calendar IS NOT NULL
			 ORDER BY created_at DESC, rowid DESC LIMIT 50`,
			p.JobName, p.jobSourceOrDefault(), p.ScheduleName)
		if err != nil {
			return false, fmt.Errorf("check recorded calendar skips: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var createdAt string
			if err := rows.Scan(&createdAt); err != nil {
				return false, fmt.Errorf("scan recorded calendar skip: %w", err)
			}
			if calendar.DayOf(parseStamp(createdAt), rec.Loc) == day {
				return true, nil
			}
		}
		return false, rows.Err()

	case dedupeEpisodeReason:
		// Keyed on the definition + entry rather than the concurrency key, and
		// matched on the reason so each suppression kind de-dupes only against
		// itself. Calendar rows are excluded for the same reason dedupeEpisode
		// excludes them: they are a different reason with a different rule.
		var status, reason string
		err := database.QueryRowContext(ctx, `
			SELECT status, COALESCE(queued_reason,'') FROM runs
			 WHERE job_name = ? AND job_source = ? AND COALESCE(schedule_name,'') = ?
			   AND suppressed_by_calendar IS NULL
			 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
			p.JobName, p.jobSourceOrDefault(), p.ScheduleName).Scan(&status, &reason)
		if err != nil {
			return false, nil // no prior run (or unreadable): record it
		}
		return status == "skipped" && reason == rec.Reason, nil

	default: // dedupeEpisode
		// Ignore calendar-suppressed rows entirely: they are a different reason
		// with a different rule, and letting one of them stand in for "the most
		// recent run" is precisely how the Forbid rule used to leak.
		var last string
		err := database.QueryRowContext(ctx, `
			SELECT status FROM runs
			 WHERE concurrency_key = ? AND suppressed_by_calendar IS NULL
			 ORDER BY created_at DESC, rowid DESC LIMIT 1`, p.ConcurrencyKey).Scan(&last)
		if err != nil {
			return false, nil // no prior run (or unreadable): record it
		}
		return last == "skipped", nil
	}
}

// recordSkippedWorkflowFire is the workflow half of the audit trail (CAL-32).
//
// recordSkippedFire cannot serve here: it is job-shaped throughout, inserting
// into `runs` with correlated subqueries against `jobs` (script_ref,
// content_hash) and `entity_codes WHERE kind='job'`. Called for a workflow it
// would mint a bogus job run named after the workflow, with every one of those
// subqueries resolving NULL.
//
// Before this, a scheduled workflow fire that did not happen left NOTHING behind
// — fireWorkflow dropped even its concurrency-cap skips with a log line. Without
// this function the whole compliance story would silently cover jobs only.
//
// workflow_runs.status has always permitted 'skipped' (migration 001), so no
// table rebuild was needed; queued_reason and suppressed_by_calendar arrive with
// migration 870.
func recordSkippedWorkflowFire(ctx context.Context, database *sql.DB, source, workflowName, scheduleName string, rec skipRecord) (bool, error) {
	dupe, err := workflowSkipAlreadyRecorded(ctx, database, source, workflowName, scheduleName, rec)
	if err != nil {
		return false, err
	}
	if dupe {
		return false, nil
	}
	if source == "" {
		source = "git"
	}
	traceID := db.NewTraceID()
	// FX-B3 — honour rec.At, as the job twin has since it was written. Stamping
	// at DETECTION time instead was invisible while the only callers were the
	// calendar and pause gates (which record as they suppress, so the two are the
	// same instant), and became load-bearing the moment the missed-run detector
	// started writing here: its markers describe fires in the PAST, and its check
	// 3 asks whether a marker exists on the same application-zone DAY as the fire.
	// A marker stamped "now" answers that question wrongly for every fire on an
	// earlier day, so a catch-up scan after an overnight outage re-recorded — and
	// re-alerted for — every enumerated fire instead of one per episode.
	now := rec.At
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	// workflow_id is left NULL: it is a nullable convenience join (migration 030)
	// and a suppressed fire has no run to correlate. workflow_name + source is the
	// identity every read path already resolves by.
	_, err = database.ExecContext(ctx, `
		INSERT INTO workflow_runs
			(id, workflow_name, workflow_source, status, queued_reason,
			 triggered_by, trigger_kind, schedule_name, suppressed_by_calendar,
			 started_at, completed_at, created_at, workflow_uid)
		VALUES (?, ?, ?, 'skipped', ?, 'scheduler', 'scheduled', ?, ?, ?, ?, ?,
			(SELECT uid FROM workflows WHERE name = ? AND source = ?))`,
		traceID, workflowName, source, rec.Reason,
		nullStr(scheduleName), nullStr(rec.Calendar), now, now, now,
		workflowName, source)
	if err != nil {
		return false, fmt.Errorf("insert skipped workflow run: %w", err)
	}
	return true, nil
}

// workflowSkipAlreadyRecorded mirrors skipAlreadyRecorded's dedupeDay and
// dedupeEpisodeReason rules, keyed on the workflow instead of the job. There is
// still no plain episode mode: workflows have no Forbid path, so no workflow
// skip ever carries a concurrency key.
func workflowSkipAlreadyRecorded(ctx context.Context, database *sql.DB, source, workflowName, scheduleName string, rec skipRecord) (bool, error) {
	if source == "" {
		source = "git"
	}
	if rec.Mode == dedupeEpisodeReason {
		var status, reason string
		err := database.QueryRowContext(ctx, `
			SELECT status, COALESCE(queued_reason,'') FROM workflow_runs
			 WHERE workflow_name = ? AND COALESCE(workflow_source,'git') = ?
			   AND COALESCE(schedule_name,'') = ?
			   AND suppressed_by_calendar IS NULL
			 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
			workflowName, source, scheduleName).Scan(&status, &reason)
		if err != nil {
			return false, nil // no prior run (or unreadable): record it
		}
		return status == "skipped" && reason == rec.Reason, nil
	}
	if rec.Mode != dedupeDay || rec.Day == "" {
		return false, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT created_at FROM workflow_runs
		 WHERE workflow_name = ? AND COALESCE(workflow_source,'git') = ?
		   AND COALESCE(schedule_name,'') = ?
		   AND status = 'skipped' AND suppressed_by_calendar IS NOT NULL
		 ORDER BY created_at DESC, rowid DESC LIMIT 50`,
		workflowName, source, scheduleName)
	if err != nil {
		return false, fmt.Errorf("check recorded workflow calendar skips: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt string
		if err := rows.Scan(&createdAt); err != nil {
			return false, fmt.Errorf("scan recorded workflow calendar skip: %w", err)
		}
		if calendar.DayOf(parseStamp(createdAt), rec.Loc) == rec.Day {
			return true, nil
		}
	}
	return false, rows.Err()
}

// parseStamp reads a stored RFC3339 timestamp. An unparseable value yields the
// zero time, whose day matches nothing — so a corrupt row makes the de-dupe
// record an extra audit entry rather than silently swallow a real suppression.
func parseStamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// EnqueueRunWithID inserts a run and returns the minted trace ID.
func EnqueueRunWithID(ctx context.Context, database *sql.DB, p EnqueueParams) (string, error) {
	traceID, err := InsertRun(ctx, database, RunRow{EnqueueParams: p})
	if err != nil {
		return "", err
	}
	// RA-20(b) — stamp WHY this run cannot be claimed, if it cannot. Advisory: it
	// never blocks the enqueue, and a runner appearing a moment later claims the run
	// normally. Every producer that goes through here gets it for free; the workflow
	// engine calls the same helper itself after its own InsertRun.
	if _, rerr := execspec.StampUnclaimableReason(ctx, database, traceID); rerr != nil {
		// A missing hint is not a failed enqueue. The run is already inserted and
		// valid; swallowing this keeps a diagnostic from becoming an outage.
		_ = rerr
	}
	if err := execspec.SyncRunAgencies(ctx, database, traceID, p.agenciesJSONOrDefault()); err != nil {
		return "", err
	}
	return traceID, nil
}

// IsForbidConflict reports whether err is a UNIQUE constraint violation on the
// runs.concurrency_key partial index (uq_runs_active_concurrency), indicating a
// concurrent Forbid-policy race was caught at the DB level (PP-L8).
func IsForbidConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "uq_runs_active_concurrency")
}

// CheckForbid returns true (conflict) if the job's Forbid policy would block
// a new run for the given concurrency key.
func CheckForbid(ctx context.Context, database *sql.DB, concKey string) (bool, error) {
	return hasActiveRunForKeyDB(ctx, database, concKey)
}

func hasActiveRunForKeyDB(ctx context.Context, database *sql.DB, concKey string) (bool, error) {
	var n int
	err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE concurrency_key = ? AND status IN ('queued','running')
	`, concKey).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Scheduler) hasActiveRunForKey(ctx context.Context, concKey string) (bool, error) {
	return hasActiveRunForKeyDB(ctx, s.db, concKey)
}

func (s *Scheduler) activeRunCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs WHERE status IN ('queued','running')
	`).Scan(&n)
	return n, err
}

func (s *Scheduler) maxConcurrent(ctx context.Context) int {
	return maxConcurrentDB(ctx, s.db)
}

func maxConcurrentDB(ctx context.Context, database *sql.DB) int {
	var val sql.NullString
	_ = database.QueryRowContext(ctx, `
		SELECT value FROM settings WHERE key = ?
	`, settingsKeyMax).Scan(&val)
	if val.Valid && val.String != "" {
		var n int
		if _, err := fmt.Sscanf(val.String, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxConcurrent
}

// nullStr converts an empty string to sql.NullString (NULL in DB).
func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// cronLogger bridges robfig/cron logging to slog.
type cronLogger struct{ log *slog.Logger }

func newCronLogger(log *slog.Logger) *cronLogger { return &cronLogger{log: log} }
func (l *cronLogger) Printf(format string, args ...any) {
	l.log.Debug(fmt.Sprintf(format, args...))
}
