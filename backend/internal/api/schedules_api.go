package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// scheduleEntryResp is one named schedule entry on a job/workflow detail
// response: the cron, optional plaintext env, and the next projected fire time
// (nil when the owning definition is paused/disabled).
type scheduleEntryResp struct {
	Name      string            `json:"name"`
	Cron      string            `json:"cron"`
	Env       map[string]string `json:"env,omitempty"`
	NextRunAt *string           `json:"nextRunAt"`
	// Name of the first-class schedule this entry was expanded from (NULL ⇒ inline,
	// job-local). Lets the composer bucket scheduleRefs vs inline entries (JC8).
	SourceRef *string `json:"sourceRef"`
	// Activation window (AW-7): RFC3339 bounds, absent when unbounded. State is
	// the derived window state — "pending" before it opens, "expired" after it
	// closes, "active" otherwise — and is empty for an unbounded entry.
	StartAt *string `json:"startAt,omitempty"`
	EndAt   *string `json:"endAt,omitempty"`
	State   string  `json:"windowState,omitempty"`
	// Interval + Mode describe the Phase 2 firing modes: an entry is cron-,
	// interval- or once-driven. Mode is derived, so a client never has to infer
	// it from which of cron/interval happens to be populated.
	Interval *string `json:"interval,omitempty"`
	Mode     string  `json:"mode,omitempty"`
}

// loadSchedules returns the schedule entries for one definition, with per-entry
// nextRunAt computed via cronutil in the APPLICATION zone (s.appLocation — the
// engine's zone; the old "time.Local" here described code two refactors gone)
// and returned as an absolute RFC3339 instant so the browser renders the
// viewer's wall clock.
// It issues no nested query, so callers must invoke it only after any outer rows
// iterator is drained (SQLite pool rule, see db.maxOpenConns).
func (s *Server) loadSchedules(ctx context.Context, ownerSource, ownerKind, ownerName string, paused bool) []scheduleEntryResp {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, cron, env, source_ref, start_at, end_at, interval FROM definition_schedules
		WHERE owner_source = ? AND owner_kind = ? AND owner_name = ?
		ORDER BY position, name
	`, ownerSource, ownerKind, ownerName)
	if err != nil {
		return nil
	}
	defer rows.Close()

	now := time.Now()
	appLoc := s.appLocation() // evaluate next-run in the same zone the engine fires in (§4.4)
	var out []scheduleEntryResp
	for rows.Next() {
		var name, cron string
		var env, sourceRef, startAt, endAt, interval sql.NullString
		if err := rows.Scan(&name, &cron, &env, &sourceRef, &startAt, &endAt, &interval); err != nil {
			continue
		}
		e := scheduleEntryResp{Name: name, Cron: cron, Env: parseEnvMap(env)}
		if sourceRef.Valid {
			e.SourceRef = &sourceRef.String
		}
		// AW-7: project through the same window the scheduler fires through, so a
		// deferred entry shows no phantom next run before it opens and an expired
		// one shows none at all.
		e.StartAt, e.EndAt = windowStrings(startAt, endAt)
		win := cronutil.NewWindow(windowTimes(startAt, endAt))
		e.State = windowState(win, now)
		spec := cronutil.Spec{Cron: cron, Interval: interval.String, Window: win}
		e.Interval, e.Mode = nullableOf(interval.String), spec.Mode()
		if !paused {
			if next, ok := cronutil.NextSpec(spec, now, appLoc); ok {
				e.NextRunAt = rfc3339Ptr(next)
			}
		}
		out = append(out, e)
	}
	return out
}

// windowState names an entry's activation-window state for the UI: "pending"
// before it opens, "expired" after it closes, "active" while inside one, and
// empty for an unbounded entry (which has no window to be in). It is orthogonal
// to enabled/paused — those are definition-level and still take precedence in
// the collapsed display status.
func windowState(win *cronutil.Window, now time.Time) string {
	switch {
	case win == nil:
		return ""
	case win.Pending(now):
		return "pending"
	case win.Expired(now):
		return "expired"
	default:
		return "active"
	}
}

// earliestNextRunAt returns the soonest non-nil nextRunAt across entries — the
// definition-level nextRunAt — or nil when nothing is scheduled. All values are
// UTC RFC3339, so a lexical min is chronological.
func earliestNextRunAt(entries []scheduleEntryResp) *string {
	var best *string
	for i := range entries {
		n := entries[i].NextRunAt
		if n == nil {
			continue
		}
		if best == nil || *n < *best {
			best = n
		}
	}
	return best
}

// listSchedules returns the schedule inventory: one row per entry across jobs and
// workflows, with enabled/paused state plus next/last run times.
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srows, err := s.drainScheduleRows(ctx)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// Preload enabled/paused state and the per-entry last-run time in a fixed
	// number of queries (CC.9) instead of three lookups per inventory entry.
	enabledSet, pausedSet := s.scheduleStates(ctx, srows)
	lastRuns := s.scheduleLastRuns(ctx, srows)

	now := time.Now()
	appLoc := s.appLocation()
	id, hasID := auth.IdentityFrom(ctx)
	items := []map[string]any{}
	// SU-2 — the roll-up is accumulated from the READABLE rows only. Building it
	// from every drained row would have leaked an out-of-scope job's NAME and its
	// compliance policy through a field the items array itself correctly hides.
	var readable []scheduleRow
	for _, sr := range srows {
		// SU-2: never expose a scoped job owner's schedule (its plaintext env) to a
		// restricted actor outside that scope.
		if !scheduleOwnerReadable(id, hasID, sr) {
			continue
		}
		readable = append(readable, sr)
		key := defKey{sr.source, sr.kind, sr.owner}
		enabled := enabledSet[key]
		paused := pausedSet[key]
		win := sr.window()
		spec := sr.spec()
		var nextRunAt *string
		if enabled && !paused {
			if next, ok := cronutil.NextSpec(spec, now, appLoc); ok {
				nextRunAt = rfc3339Ptr(next)
			}
		}
		var lastRunAt *string
		if v, ok := lastRuns[runKey{sr.source, sr.kind, sr.owner, sr.name}]; ok {
			vv := v
			lastRunAt = &vv
		}
		startAt, endAt := windowStrings(sr.startAt, sr.endAt)
		items = append(items, map[string]any{
			"ownerKind":     sr.kind,
			"ownerName":     sr.owner,
			"ownerUid":      sr.ownerUID.String,
			"ownerAgencies": splitAgencies(sr.ownerAgencies),
			"ownerSource":   sr.source,
			"scheduleName":  sr.name,
			"cron":          sr.cron,
			"env":           parseEnvMap(sr.env),
			"enabled":       enabled,
			"paused":        paused,
			"nextRunAt":     nextRunAt,
			"lastRunAt":     lastRunAt,
			// AW-7 — the window is orthogonal to enabled/paused: those are
			// definition-level and still win in the collapsed display status.
			"startAt":     startAt,
			"endAt":       endAt,
			"windowState": windowState(win, now),
			"interval":    nullableOf(sr.interval.String),
			"mode":        spec.Mode(),
			// CAL-5 — the entry's own bindings, so the Inventory row can show what
			// this entry is bound to rather than only the owner-level roll-up.
			"skipCalendars": calendar.ParseNames(sr.skipCals.String),
			"onlyCalendars": calendar.ParseNames(sr.onlyCals.String),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items,
		// CAL-12 — the per-owner roll-up that answers CAL-Q1's audit objection.
		"calendarRollup": calendarRollup(readable),
	})
}

// calendarRollup answers "does job X run on holidays?" in ONE place, which is the
// cost entry-level binding (§2.5) otherwise imposes: the question would mean
// checking every entry of X rather than one field.
//
// Keyed "kind:source:name". Entries that AGREE collapse to the bare calendar
// names; entries that disagree render "federal-holidays (2 of 3 entries)". The
// disagreement case is the one worth surfacing loudly — a definition-level field
// would have made it unrepresentable rather than visible, which is precisely the
// wrong trade in a compliance context.
//
// Entries with NO binding still count toward the denominator: an entry that skips
// nothing is exactly the entry an auditor is looking for.
func calendarRollup(srows []scheduleRow) map[string]any {
	type acc struct {
		total  int
		counts map[string]int
	}
	byOwner := map[string]*acc{}
	for _, sr := range srows {
		key := sr.kind + ":" + sr.source + ":" + sr.owner
		a, ok := byOwner[key]
		if !ok {
			a = &acc{counts: map[string]int{}}
			byOwner[key] = a
		}
		a.total++
		seen := map[string]bool{}
		for _, n := range append(calendar.ParseNames(sr.skipCals.String), calendar.ParseNames(sr.onlyCals.String)...) {
			if seen[n] {
				continue // one entry naming a calendar twice is still one entry
			}
			seen[n] = true
			a.counts[n]++
		}
	}
	out := map[string]any{}
	for key, a := range byOwner {
		if len(a.counts) == 0 {
			continue // no bindings anywhere on this definition: nothing to say
		}
		names := make([]string, 0, len(a.counts))
		for n := range a.counts {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			if a.counts[n] == a.total {
				parts = append(parts, n)
				continue
			}
			parts = append(parts, fmt.Sprintf("%s (%d of %d entries)", n, a.counts[n], a.total))
		}
		out[key] = strings.Join(parts, ", ")
	}
	return out
}

// listUpcomingSchedules projects each enabled, unpaused entry's fire times within
// the window (24h default, or 7d), flattened soonest-first and capped.
func (s *Server) listUpcomingSchedules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window := r.URL.Query().Get("window")
	horizon := 24 * time.Hour
	if window == "7d" {
		horizon = 7 * 24 * time.Hour
	} else {
		window = "24h"
	}

	srows, err := s.drainScheduleRows(ctx)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	const perEntryCap = 200
	const totalCap = 500
	now := time.Now()
	end := now.Add(horizon)
	appLoc := s.appLocation()

	// Preload enabled/paused state in a fixed number of queries (CC.9).
	enabledSet, pausedSet := s.scheduleStates(ctx, srows)
	id, hasID := auth.IdentityFrom(ctx)

	// CAL-10 — the projection must consult the SAME resolver the engine does, or
	// the product lies twice: it promises a run that will not happen, and it hides
	// the holiday gap the operator specifically wants to see. Every calendar named
	// by any entry is loaded ONCE here (plus every global calendar, which Load
	// always includes), preserving the fixed-query-count discipline (CC.9) — never
	// one Load per entry, let alone per projected instant.
	var allCalNames []string
	for _, sr := range srows {
		allCalNames = append(allCalNames, calendar.ParseNames(sr.skipCals.String)...)
		allCalNames = append(allCalNames, calendar.ParseNames(sr.onlyCals.String)...)
	}
	calSets, calErr := calendar.Load(ctx, s.db, allCalNames)
	if calErr != nil {
		// Degrade to an unannotated projection rather than failing the endpoint:
		// the Upcoming tab going blank is worse than it briefly over-promising.
		s.log.Error("upcoming: load calendars — projection not annotated", "err", calErr)
		calSets = calendar.Set{}
	}

	type proj struct {
		kind, owner, name, cron string
		source                  string
		// R2F-3 — carried through the projection so an upcoming instant can
		// qualify an owner name two departments share.
		ownerUID      string
		ownerAgencies []string
		at            time.Time
		verdict       calendar.Verdict
	}
	var projections []proj
	for _, sr := range srows {
		// SU-2: restricted actors must not see out-of-scope job owners' schedules.
		if !scheduleOwnerReadable(id, hasID, sr) {
			continue
		}
		key := defKey{sr.source, sr.kind, sr.owner}
		if !enabledSet[key] || pausedSet[key] {
			continue
		}
		// AW-7: a pending entry contributes nothing before its window opens, and
		// an expired one contributes nothing at all — matching real fires.
		skipCals := calendar.ParseNames(sr.skipCals.String)
		onlyCals := calendar.ParseNames(sr.onlyCals.String)
		for _, t := range cronutil.NextNSpec(sr.spec(), now, end, perEntryCap, appLoc) {
			// Evaluated per instant, in the app zone, exactly as fire() will —
			// a holiday suppresses one day's instant, not the whole entry.
			v := calendar.Evaluate(calSets, skipCals, onlyCals, calendar.DayOf(t, appLoc))
			projections = append(projections, proj{sr.kind, sr.owner, sr.name, sr.cron, sr.source,
				sr.ownerUID.String, splitAgencies(sr.ownerAgencies), t, v})
		}
	}
	sort.Slice(projections, func(i, j int) bool { return projections[i].at.Before(projections[j].at) })
	if len(projections) > totalCap {
		projections = projections[:totalCap]
	}

	items := make([]map[string]any, 0, len(projections))
	for _, p := range projections {
		item := map[string]any{
			"ownerKind":     p.kind,
			"ownerName":     p.owner,
			"ownerUid":      p.ownerUID,
			"ownerAgencies": p.ownerAgencies,
			"ownerSource":   p.source,
			"scheduleName":  p.name,
			"cron":          p.cron,
			"at":            p.at.UTC().Format(time.RFC3339),
		}
		// Suppressed instants are ANNOTATED, never dropped (CAL-10). Dropping them
		// server-side would have been less work and would have thrown away the one
		// view where a holiday gap is worth seeing; the Dashboard filters them out
		// of "next up" instead (CAL-11), because a suppressed fire is not an
		// upcoming run even though it IS a thing to show on a timeline.
		if p.verdict.Suppressed {
			item["suppressed"] = true
			item["suppressedBy"] = p.verdict.Calendar
			if p.verdict.Label != "" {
				item["suppressedLabel"] = p.verdict.Label
			}
		}
		items = append(items, item)
	}

	// AR — union in deferred ad-hoc runs (pending_runs), so this endpoint stays
	// THE answer to "what will fire, and when". They are flagged adHoc (with the
	// pending id, so the UI can offer Cancel) and interleaved by instant.
	// 'missed' rows ride a separate key: they are not upcoming, but silently
	// hiding them would defeat the point of keeping them.
	adhoc, missed := s.pendingRunItems(ctx, id, hasID, now, end)
	if len(adhoc) > 0 {
		items = append(items, adhoc...)
		sort.Slice(items, func(i, j int) bool {
			return items[i]["at"].(string) < items[j]["at"].(string)
		})
		if len(items) > totalCap {
			items = items[:totalCap]
		}
	}

	resp := map[string]any{"window": window, "items": items}
	if len(missed) > 0 {
		resp["missedAdHoc"] = missed
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// scheduleRow is a drained definition_schedules row, scanned before any per-row
// status lookups (which would deadlock if run with the iterator still open).
// ownerScope is the resolved scope of a JOB owner (definition_schedules has no
// scope column of its own, SU-2); it is NULL for workflow owners and global jobs,
// which have no scope and are readable by everyone.
type scheduleRow struct {
	source, kind, owner, name, cron string
	env                             sql.NullString
	ownerScope                      sql.NullString
	// R2F-3 — the owner's identity and derived agencies, so a surface listing
	// owners by name can qualify one that two departments share. ownerAgencies
	// is resolved for JOB owners only (a workflow has no scope of its own; its
	// agencies are the union over its jobs, which needs step parsing and is
	// resolved by the handlers that care).
	ownerUID           sql.NullString
	ownerAgencies      sql.NullString
	startAt, endAt     sql.NullString // activation window (AW-7)
	interval           sql.NullString // anchored-interval mode (Phase 2)
	skipCals, onlyCals sql.NullString // working-calendar bindings (CAL-10)
}

// window builds the entry's activation window for projection.
func (sr scheduleRow) window() *cronutil.Window {
	return cronutil.NewWindow(windowTimes(sr.startAt, sr.endAt))
}

// spec builds the entry's firing rule in whichever mode it was authored, so the
// projections agree with the engine for cron, interval and once alike.
func (sr scheduleRow) spec() cronutil.Spec {
	return cronutil.Spec{Cron: sr.cron, Interval: sr.interval.String, Window: sr.window()}
}

// binnedOwnerSQL is TRUE when a definition_schedules row (aliased `ds`) is
// owned by a recycle-binned definition — the one rule "a binned owner's
// schedule entries are not live" (FX-A4), stated once (FX2-F2). Interpolate it
// rather than hand-copying: drainScheduleRows filters on NOT it, the calendar
// binding collector selects it as a flag, and the next reader of
// definition_schedules who forgets it silently re-opens the FX-A4 hole
// (phantom bindings and promised runs for binned owners) — the exact drift
// that produced FX-A4 in the first place.
const binnedOwnerSQL = `(EXISTS (SELECT 1 FROM jobs j2
                   WHERE ds.owner_kind = 'job' AND j2.source = ds.owner_source
                     AND j2.name = ds.owner_name AND j2.deleted_at IS NOT NULL)
              OR EXISTS (SELECT 1 FROM workflows w2
                   WHERE ds.owner_kind = 'workflow' AND w2.source = ds.owner_source
                     AND w2.name = ds.owner_name AND w2.deleted_at IS NOT NULL))`

func (s *Server) drainScheduleRows(ctx context.Context) ([]scheduleRow, error) {
	// SU-2: resolve each schedule's owner scope so the read handlers can drop
	// out-of-scope rows (they otherwise expose a scoped job owner's plaintext env).
	// The correlated subquery only fires for job owners (workflows have no scope),
	// and it is part of this single SELECT — no extra round-trip, so the schedule
	// handlers' query budget (schedules_batch_test) is unchanged.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ds.owner_source, ds.owner_kind, ds.owner_name, ds.name, ds.cron, ds.env,
		       (SELECT j.scope FROM jobs j
		         WHERE ds.owner_kind = 'job' AND j.source = ds.owner_source AND j.name = ds.owner_name) AS owner_scope,
		       ds.start_at, ds.end_at, ds.interval, ds.skip_calendars, ds.only_calendars,
		       -- R2F-3: the owner's identity (stamped since 1020) and, for job
		       -- owners, its derived agencies — both in THIS select, so the
		       -- schedule handlers' query budget (schedules_batch_test) is
		       -- unchanged, exactly as owner_scope above.
		       ds.owner_uid,
		       (SELECT GROUP_CONCAT(a.name, '\x1f')
		          FROM jobs j
		          JOIN scopes sc         ON sc.name = j.scope
		          JOIN scope_agencies sa ON sa.scope_id = sc.id
		          JOIN agencies a        ON a.id = sa.agency_id
		         WHERE ds.owner_kind = 'job' AND j.source = ds.owner_source AND j.name = ds.owner_name) AS owner_agencies
		FROM definition_schedules ds
		-- FX-A4: a binned owner's entries survive the soft delete (that is what
		-- makes a restore cheap) but the scheduler's reload skips them, so listing
		-- them promised runs that could not happen. Filtered here, at the single
		-- drain every schedule read goes through, rather than per-handler.
		WHERE NOT `+binnedOwnerSQL+`
		ORDER BY ds.owner_source, ds.owner_kind, ds.owner_name, ds.position, ds.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduleRow
	for rows.Next() {
		var sr scheduleRow
		if err := rows.Scan(&sr.source, &sr.kind, &sr.owner, &sr.name, &sr.cron, &sr.env, &sr.ownerScope,
			&sr.startAt, &sr.endAt, &sr.interval, &sr.skipCals, &sr.onlyCals,
			&sr.ownerUID, &sr.ownerAgencies); err != nil {
			continue
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// scheduleOwnerReadable reports whether the actor may see a drained schedule row,
// given its resolved owner scope (SU-2). Unrestricted actors, workflow-owned
// schedules, and global (NULL-scope) job owners are always readable.
func scheduleOwnerReadable(id auth.Identity, hasID bool, sr scheduleRow) bool {
	// Fail closed: only an unrestricted actor sees everything; anyone else (restricted,
	// or — unreachable behind RequireSession — no identity) is gated by ScopeReadable,
	// which still admits NULL/global owner scopes.
	if hasID && id.Unrestricted() {
		return true
	}
	scope := ""
	if sr.ownerScope.Valid {
		scope = sr.ownerScope.String
	}
	return auth.ScopeReadable(id, scope)
}

// defKey identifies a job/workflow definition for the batched schedule-state
// preloads (CC.9): (source, owner_kind, name).
type defKey struct{ source, kind, name string }

// runKey identifies a (definition, schedule) pair for the batched last-run
// preload. sched is COALESCE(schedule_name,'default'), so the 'default' bucket
// also holds legacy NULL-schedule runs (see scheduleLastRuns).
type runKey struct{ source, kind, owner, sched string }

// scheduleStates preloads the enabled and paused state of every definition
// referenced by srows in a fixed number of queries (CC.9), replacing the
// per-entry definitionEnabled/schedulePaused lookups. A missing enabled entry
// reads as false (definition absent or disabled), keyed by (source, kind, name)
// so a job and a workflow that share a name never collide.
func (s *Server) scheduleStates(ctx context.Context, srows []scheduleRow) (enabled, paused map[defKey]bool) {
	enabled = map[defKey]bool{}
	paused = map[defKey]bool{}
	jobNames, wfNames := distinctScheduleOwners(srows)

	loadEnabled := func(table, kind string, names []any) {
		if len(names) == 0 {
			return
		}
		ph := strings.Repeat(",?", len(names))[1:]
		// FX-A4: a binned definition reads as not-enabled. drainScheduleRows already
		// drops its entries, so this is defense in depth for any future caller that
		// assembles srows another way.
		rows, err := s.db.QueryContext(ctx,
			`SELECT source, name, enabled FROM `+table+` WHERE deleted_at IS NULL AND name IN (`+ph+`)`, names...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var src, nm string
			var en int
			if rows.Scan(&src, &nm, &en) == nil {
				enabled[defKey{src, kind, nm}] = en == 1
			}
		}
	}
	loadEnabled("jobs", "job", jobNames)
	loadEnabled("workflows", "workflow", wfNames)

	// paused_jobs holds only currently-paused definitions, so scanning it whole
	// is cheap and avoids an IN list.
	prows, err := s.db.QueryContext(ctx, `SELECT source, owner_kind, name FROM paused_jobs`)
	if err == nil {
		defer prows.Close()
		for prows.Next() {
			var src, kind, nm string
			if prows.Scan(&src, &kind, &nm) == nil {
				paused[defKey{src, kind, nm}] = true
			}
		}
	}
	return enabled, paused
}

// scheduleLastRuns preloads the most-recent run timestamp per (definition,
// schedule) in two grouped queries (CC.9). COALESCE(schedule_name,'default')
// reproduces scheduleLastRunAt's rule that the 'default' entry also matches
// legacy runs with a NULL schedule_name, and MAX(created_at) is the same
// newest-first pick as the old ORDER BY created_at DESC LIMIT 1.
func (s *Server) scheduleLastRuns(ctx context.Context, srows []scheduleRow) map[runKey]string {
	out := map[runKey]string{}
	jobNames, wfNames := distinctScheduleOwners(srows)

	load := func(table, sourceCol, nameCol, kind string, names []any) {
		if len(names) == 0 {
			return
		}
		ph := strings.Repeat(",?", len(names))[1:]
		// FX-D1 — EXECUTED runs only, matching the Jobs and Workflows lists. This
		// column is "Last run" on the Schedules inventory; counting suppressions
		// made it answer "when did we last DECLINE to fire", so the two pages
		// disagreed about the same job on any day a calendar had vetoed it.
		rows, err := s.db.QueryContext(ctx, `SELECT `+sourceCol+`, `+nameCol+`, COALESCE(schedule_name,'default'), MAX(created_at)
			FROM `+table+` WHERE `+nameCol+` IN (`+ph+`) AND status <> 'skipped'
			GROUP BY `+sourceCol+`, `+nameCol+`, COALESCE(schedule_name,'default')`, names...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var src, owner, sched string
			var maxAt sql.NullString
			if rows.Scan(&src, &owner, &sched, &maxAt) == nil && maxAt.Valid {
				out[runKey{src, kind, owner, sched}] = maxAt.String
			}
		}
	}
	load("runs", "job_source", "job_name", "job", jobNames)
	load("workflow_runs", "workflow_source", "workflow_name", "workflow", wfNames)
	return out
}

// distinctScheduleOwners splits the schedule rows' owner names into deduplicated
// job and workflow buckets for the IN clauses of the batched preloads.
func distinctScheduleOwners(srows []scheduleRow) (jobNames, wfNames []any) {
	jobSeen, wfSeen := map[string]bool{}, map[string]bool{}
	for _, sr := range srows {
		if sr.kind == "workflow" {
			if !wfSeen[sr.owner] {
				wfSeen[sr.owner] = true
				wfNames = append(wfNames, sr.owner)
			}
		} else {
			if !jobSeen[sr.owner] {
				jobSeen[sr.owner] = true
				jobNames = append(jobNames, sr.owner)
			}
		}
	}
	return jobNames, wfNames
}

// parseEnvMap unmarshals a JSON object string into a map (nil when absent/blank).
func parseEnvMap(env sql.NullString) map[string]string {
	if !env.Valid || env.String == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(env.String), &m); err != nil {
		return nil
	}
	return m
}

// rfc3339Ptr formats t as an absolute UTC RFC3339 instant and returns a pointer.
func rfc3339Ptr(t time.Time) *string {
	v := t.UTC().Format(time.RFC3339)
	return &v
}

// pendingRunItems returns the deferred ad-hoc runs due within (now, end] as
// upcoming items, plus any 'missed' rows (regardless of instant — they are a
// standing notice until cancelled). Job rows are filtered by the caller's scope
// grants (SU-2), using the scope frozen on the pending row.
func (s *Server) pendingRunItems(ctx context.Context, id auth.Identity, hasID bool, now, end time.Time) (upcoming, missed []map[string]any) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, source, scope, run_at, scheduled_by, status, COALESCE(miss_reason,''),
		       COALESCE(gate_kind,''), COALESCE(concurrency_key,''), COALESCE(origin_kind,'')
		FROM pending_runs ORDER BY run_at ASC`)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	for rows.Next() {
		var rowID, kind, name, source, runAt, schedBy, status, missReason, gateKind, concKey, originKind string
		var scope sql.NullString
		if err := rows.Scan(&rowID, &kind, &name, &source, &scope, &runAt, &schedBy, &status, &missReason,
			&gateKind, &concKey, &originKind); err != nil {
			continue
		}
		// Same fail-closed rule as scheduleOwnerReadable: only an unrestricted
		// actor sees everything; anyone else is gated by ScopeReadable (which
		// still admits NULL/global scopes).
		if !(hasID && id.Unrestricted()) && !auth.ScopeReadable(id, scope.String) {
			continue
		}
		item := map[string]any{
			"ownerKind":   kind,
			"ownerName":   name,
			"ownerSource": source,
			"at":          runAt,
			// FX-B2: not every parked row is an operator's deferral. A Queue-policy
			// hold is a CRON fire waiting on its key, and labelling it ad-hoc sent
			// the reader looking for a person who scheduled it.
			// FX2-D3: PROVENANCE tells them apart, not gate-state — the SQL twin of
			// pendingRow.isAdHoc. The hold stamps ('recycle_bin', 'gate:*') say why
			// a row is CURRENTLY waiting, not who parked it: an operator's deferral
			// that is then paused (or its job binned) is still the operator's
			// deferral, and flipping its label sent the reader hunting for a cron
			// entry that does not exist. A held ad-hoc row carries BOTH adHoc:true
			// and waitingOn, which is the truthful state.
			"adHoc":       originKind != "reaction" && gateKind != "concurrency",
			"pendingId":   rowID,
			"scheduledBy": schedBy,
		}
		if status == "missed" {
			item["missReason"] = missReason
			missed = append(missed, item)
			continue
		}
		// QP — a gate-queued row needs its own branch or it is invisible here.
		// Its run_at is its INSERT time, so it is already in the past, and the
		// window filter below admits only future instants: a queued run would
		// appear in neither list while sitting perfectly alive in the queue.
		if gateKind == "concurrency" {
			item["queued"] = true
			item["concurrencyKey"] = concKey
			item["waitingOn"] = "concurrency"
			upcoming = append(upcoming, item)
			continue
		}
		// FX-A3 — a row held because its owner is in the recycle bin needs the same
		// branch for the same reason: its run_at is in the past, so the window
		// filter below would drop it and the operator would see a run they can
		// neither find nor cancel (Cancel needs the pendingId only this endpoint
		// carries). It is alive and waiting on a restore, not late.
		if gateKind == scheduler.GateRecycleBin {
			item["queued"] = true
			item["waitingOn"] = "recycleBin"
			upcoming = append(upcoming, item)
			continue
		}
		// FX-C — a row held by an admission gate (pause, a fleet-wide freeze) needs
		// the same branch for the same reason: its run_at is already past, so the
		// window filter below would drop it and the operator would see a run they
		// can neither find nor cancel. It is alive and waiting on a condition that
		// can clear, not late.
		if strings.HasPrefix(gateKind, scheduler.GateHoldPrefix) {
			item["queued"] = true
			item["waitingOn"] = strings.TrimPrefix(gateKind, scheduler.GateHoldPrefix)
			upcoming = append(upcoming, item)
			continue
		}
		if at, err := time.Parse(time.RFC3339, runAt); err == nil && at.After(now) && !at.After(end) {
			upcoming = append(upcoming, item)
		}
	}
	return upcoming, missed
}
