package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountScheduleCompose owns the in-app first-class Schedule authoring write surface
// (schedule-builder.md — it closes the v20 Phase-1 deferral: operator authoring of
// first-class Schedules). It is the schedule analog of mountJobCompose /
// mountWorkflowCompose: an cronomicon-source Schedule is a DB-authored named cron (+
// optional env) created/edited/deleted in-app — no Git round-trip. Git-source
// schedules are read-only here (409); they round-trip through the Git publish flow.
//
// Addressed by {name} (D2): cronomicon schedule names are unique within source, and git
// rows are read-only, so writes never need ?source disambiguation. A schedule edit
// PROPAGATES to every job/workflow that referenced it (D1c) via the source_ref
// column, so a cron change actually takes effect rather than going stale in the
// catalog while referencing definitions keep firing the old cron.
//
// Routes (all behind requireCompose — Admin-only, the same gate as Job/Workflow compose):
//
//	POST   /api/v1/schedule-defs           create an cronomicon-source schedule
//	PUT    /api/v1/schedule-defs/{name}    edit an cronomicon-source schedule (409 on git, 404 if absent)
//	DELETE /api/v1/schedule-defs/{name}    delete an cronomicon-source schedule (409 when referenced unless ?force=true)
//
// 🔴 RB-30: these three routes are the schedule-authoring surface, and their
// admin-only gate is what makes RB-Q11(c) — "a scheduled fire of an unscoped job
// runs unbound, with no scope check" — a non-issue rather than a bypass of RB-26.
// Do not widen them without reading requireCompose's note in job_compose_mount.go.
func (s *Server) mountScheduleCompose(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/schedule-defs", s.requireComposeAdmin(http.HandlerFunc(s.createScheduleDef)))
	mux.Handle("PUT /api/v1/schedule-defs/{name}", s.requireComposeAdmin(http.HandlerFunc(s.updateScheduleDef)))
	mux.Handle("DELETE /api/v1/schedule-defs/{name}", s.requireComposeAdmin(http.HandlerFunc(s.deleteScheduleDef)))
}

// scheduleInput is the request body for create/update (the ScheduleInput schema).
type scheduleInput struct {
	Name        string            `json:"name"`
	Cron        string            `json:"cron"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description"`
	// StartAt/EndAt are the optional activation window (AW-4): RFC3339 bounds on
	// when the cron may fire. Absent/null on both sides is the pre-window
	// behavior — active immediately, never expires.
	StartAt *string `json:"startAt,omitempty"`
	EndAt   *string `json:"endAt,omitempty"`
	// Interval is the Phase 2 anchored-interval mode ("7d", "36h"): mutually
	// exclusive with Cron, and requires StartAt as its phase anchor. Leaving
	// both Cron and Interval empty with a StartAt set is the one-shot mode.
	Interval *string `json:"interval,omitempty"`
	// SkipCalendars / OnlyCalendars are the working-calendar bindings (CAL-5).
	// On a FIRST-CLASS schedule they are the whole reusable-policy story (§2.5):
	// "weeknights-non-holiday" is a complete policy object, and binding the ref
	// to forty jobs gives all forty the calendar from one place.
	SkipCalendars []string `json:"skipCalendars,omitempty"`
	OnlyCalendars []string `json:"onlyCalendars,omitempty"`
}

// specOf builds the cronutil spec an input describes, so validation and storage
// agree on the mode without either re-deriving it.
func (in scheduleInput) specOf(startAt, endAt *string) cronutil.Spec {
	return cronutil.Spec{
		Cron:     strings.TrimSpace(in.Cron),
		Interval: strings.TrimSpace(deref(in.Interval)),
		Window:   cronutil.NewWindow(windowBound(nullStringOf(startAt)), windowBound(nullStringOf(endAt))),
	}
}

func (s *Server) createScheduleDef(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in scheduleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !composeNameRe.MatchString(in.Name) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid schedule name (want "+composeNameRe.String()+")")
		return
	}
	s.writeComposedSchedule(w, r, in, id.Email, true)
}

func (s *Server) updateScheduleDef(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	name := r.PathValue("name")
	switch s.scheduleSourceState(r.Context(), name) {
	case scheduleAbsent:
		httpx.Fail(w, http.StatusNotFound, "not_found", "schedule not found")
		return
	case scheduleGitOnly:
		httpx.Fail(w, http.StatusConflict, "conflict", "only cronomicon-source schedules are editable in-app; git schedules round-trip through the publish flow")
		return
	}
	if deleted, err := s.definitionIsDeleted(r.Context(), revKindSchedule, name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	} else if deleted {
		httpx.Fail(w, http.StatusConflict, "conflict",
			"this schedule is in the recycle bin — restore it before editing")
		return
	}
	var in scheduleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = name // name is the identity — immutable on edit
	s.writeComposedSchedule(w, r, in, id.Email, false)
}

// writeComposedSchedule is the single write body for cronomicon-source schedules,
// shared by create, update and revision-restore.
//
// It exists because there wasn't one. Create and update each hand-rolled their
// own SQL, and create ran its INSERT on s.db with NO TRANSACTION at all — which
// was survivable while it was a lone statement and stops being so the moment a
// revision snapshot has to commit atomically with it. Jobs and Workflows have
// had writeComposed* since A11; this is Schedules catching up, and the tx is a
// latent-bug fix independent of RH.
func (s *Server) writeComposedSchedule(w http.ResponseWriter, r *http.Request, in scheduleInput, actor string, isCreate bool) {
	cron := strings.TrimSpace(in.Cron)
	if err := envref.ValidateOperatorEnv(in.Env); err != nil { // W4/N-D1 reserved-namespace guard
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	startAt, endAt, werr := parseWindowPair(in.StartAt, in.EndAt)
	if werr != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", werr.Error())
		return
	}
	// One validation for all three modes (cron / interval / once), shared with
	// the Git YAML path so both authoring surfaces enforce the same contract.
	interval := strings.TrimSpace(deref(in.Interval))
	if serr := in.specOf(startAt, endAt).Validate(); serr != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", serr.Error())
		return
	}
	skipCals, onlyCals, cerr := s.validateCalendarBinding(r.Context(), in.SkipCalendars, in.OnlyCalendars)
	if cerr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", cerr)
		return
	}

	if isCreate {
		// Disjoint namespaces (Q-A): only an existing CRONOMICON schedule of this name
		// conflicts; a git schedule of the same name may coexist. A binned one
		// still holds its name — say so, or the operator has no way to know why.
		var exists int
		var deleted sql.NullString
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*), MAX(deleted_at) FROM schedules WHERE source='cronomicon' AND name=?`,
			in.Name).Scan(&exists, &deleted)
		if exists > 0 {
			msg := "an cronomicon schedule with this name already exists"
			if deleted.Valid {
				msg = "a schedule with this name is in the recycle bin — restore or purge it to reuse the name"
			}
			httpx.Fail(w, http.StatusConflict, "conflict", msg)
			return
		}
	}

	envJSON := scheduleEnvJSON(in.Env)
	now := time.Now().UTC().Format(time.RFC3339)
	hash := gitlab.ScheduleContentHash(cron, in.Env, deref(startAt), deref(endAt), interval, skipCals, onlyCals)

	// Owners that reference this schedule (by source_ref) — captured before the tx
	// for the legacy-mirror recompute. The owner set is stable across an edit (no
	// rows added/removed; only cron/env change), so a pre-tx read is correct.
	var owners []scheduleOwner
	if !isCreate {
		var err error
		owners, err = collectScheduleOwners(r.Context(), s.db, in.Name)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()

	if isCreate {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO schedules(name, source, description, cron, env, content_hash,
			                      start_at, end_at, interval, skip_calendars, only_calendars,
			                      created_by, created_at, last_modified_by, last_modified_at, uid)
			VALUES(?, 'cronomicon', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Name, nullStrIf(in.Description), cron, envJSON, hash,
			windowArg(startAt), windowArg(endAt), nullStrIf(interval),
			nullStrIf(calendar.MarshalNames(skipCals)), nullStrIf(calendar.MarshalNames(onlyCals)),
			actor, now, actor, now, db.NewID()); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	} else {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE schedules SET cron=?, env=?, description=?, content_hash=?,
			                     start_at=?, end_at=?, interval=?,
			                     skip_calendars=?, only_calendars=?,
			                     last_modified_by=?, last_modified_at=?
			WHERE source='cronomicon' AND name=?`,
			cron, envJSON, nullStrIf(in.Description), hash,
			windowArg(startAt), windowArg(endAt), nullStrIf(interval),
			nullStrIf(calendar.MarshalNames(skipCals)), nullStrIf(calendar.MarshalNames(onlyCals)),
			actor, now, in.Name); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// Propagate to every referencing job/workflow (D1c). source_ref makes this
		// exact — an inline entry that coincidentally shares the name is untouched.
		// The activation window rides along with cron/env (AW-4): a ref expansion is
		// a copy of the catalog row, so an edited window must reach the runtime
		// entries or the catalog and the scheduler would disagree.
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE definition_schedules SET cron=?, env=?, start_at=?, end_at=?, interval=?,
			                                 skip_calendars=?, only_calendars=? WHERE source_ref=?`,
			cron, envJSON, windowArg(startAt), windowArg(endAt), nullStrIf(interval),
			nullStrIf(calendar.MarshalNames(skipCals)), nullStrIf(calendar.MarshalNames(onlyCals)), in.Name); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// Keep the legacy display mirror (jobs/workflows.schedule = lowest-position
		// cron, §3.7) consistent for owners whose position-0 entry is this schedule.
		for _, o := range owners {
			if err := recomputeLegacyMirror(r.Context(), tx, o.source, o.kind, o.name); err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
		}
	}

	// RH — the snapshot commits with the write it describes, never after it.
	action := revActionUpdated
	if isCreate {
		action = revActionCreated
	}
	var schedUID string
	_ = tx.QueryRowContext(r.Context(), `SELECT uid FROM schedules WHERE source='cronomicon' AND name=?`, in.Name).Scan(&schedUID)
	if err := snapshotRevision(r.Context(), tx, revKindSchedule, "cronomicon", in.Name, schedUID, actor, action, in); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	auditAction := "Updated"
	status := http.StatusOK
	if isCreate {
		auditAction, status = "Created", http.StatusCreated
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Schedules", auditAction, in.Name, "Cronomicon schedule "+strings.ToLower(auditAction))
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Schedules", Action: auditAction, Target: in.Name,
	})
	// A bare create references no definition_schedules, so a reload is a cheap
	// no-op — but harmless, and keeps the seam consistent with edit/delete (D8).
	s.forceScheduleReload(r.Context())
	s.writeScheduleDef(w, r, in.Name, status)
}

func (s *Server) deleteScheduleDef(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	name := r.PathValue("name")
	switch s.scheduleSourceState(r.Context(), name) {
	case scheduleAbsent:
		httpx.Fail(w, http.StatusNotFound, "not_found", "schedule not found")
		return
	case scheduleGitOnly:
		httpx.Fail(w, http.StatusConflict, "conflict", "only cronomicon-source schedules are deletable in-app")
		return
	}
	force := r.URL.Query().Get("force") == "true"

	// Capture referrers BEFORE deletion: needed for both the block-by-default 409
	// and (when forced) the legacy-mirror recompute after the cascade detaches them.
	owners, err := collectScheduleOwners(r.Context(), s.db, name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// FX-A4 — the BLOCK is judged on live referrers only, while `owners` above
	// keeps the binned ones for the mirror recompute below: a definition in the
	// recycle bin cannot fire, so letting it hold a schedule hostage made the
	// operator pass ?force=true to detach nothing. The two consumers genuinely
	// want different sets, which is why this filters here and not in the collector.
	blocking, err := liveScheduleOwners(r.Context(), s.db, owners)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if len(blocking) > 0 && !force {
		refs := make([]string, 0, len(blocking))
		for _, o := range blocking {
			refs = append(refs, o.kind+":"+o.name)
		}
		httpx.Fail(w, http.StatusConflict, "conflict",
			"schedule is referenced by "+strconv.Itoa(len(blocking))+" definition(s): "+strings.Join(refs, ", ")+" — pass ?force=true to delete and detach them")
		return
	}

	// RH — SOFT delete. Unlike jobs and workflows a schedule's runtime expansions
	// are keyed by source_ref rather than cascaded by a trigger, so they are
	// detached here as before: a binned schedule must stop firing its referrers,
	// and the entries are rebuilt from the catalog row on restore.
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(r.Context(),
		`UPDATE schedules SET deleted_at=?, deleted_by=? WHERE source='cronomicon' AND name=? AND deleted_at IS NULL`,
		now, id.Email, name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "this schedule is already in the recycle bin")
		return
	}
	// Cascade the ref-expanded runtime entries (exact, by source_ref — never an
	// inline entry of the same name). These rows ARE the referrers' record that
	// they reference this schedule — nothing else stores it — so they are read
	// into the tombstone below before being removed, and restore replays them.
	detached, err := collectScheduleBindings(r.Context(), tx, name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM definition_schedules WHERE source_ref=?`, name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// Resync the legacy display column for each detached owner to its next remaining
	// schedule cron (or NULL), so the UI shows no stale anomaly (D3 Legacy Column Sync).
	for _, o := range owners {
		if err := recomputeLegacyMirror(r.Context(), tx, o.source, o.kind, o.name); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}
	var delSchedUID string
	_ = tx.QueryRowContext(r.Context(), `SELECT uid FROM schedules WHERE source='cronomicon' AND name=?`, name).Scan(&delSchedUID)
	if err := snapshotRevision(r.Context(), tx, revKindSchedule, "cronomicon", name, delSchedUID, id.Email, revActionDeleted,
		map[string]any{"deletedAt": now, "deletedBy": id.Email, "detachedBindings": detached}); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// FX2-C2 — a delete that proceeded past BINNED referrers says so where an
	// operator will read it. The delete itself is soft and the bindings sit in
	// the tombstone (restore replays them, binned owners included, per FX-Q3),
	// so no 409 — but "deleted silently while job:J still referenced it from the
	// bin" is exactly the sequence that reads as data loss a week later, and the
	// Activity line is the only record of who to ask. Binned referrers are the
	// owners the 409 above did NOT count: owners minus blocking.
	detail := "Cronomicon schedule deleted"
	if binned := diffOwners(owners, blocking); len(binned) > 0 {
		refs := make([]string, 0, len(binned))
		for _, o := range binned {
			refs = append(refs, o.kind+":"+o.name)
		}
		detail += " — detached " + strconv.Itoa(len(binned)) + " binding(s) held by recycle-binned " +
			strings.Join(refs, ", ") + "; restore the schedule to recover them"
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Schedules", "Deleted", name, detail)
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: id.Email, Category: "Schedules", Action: "Deleted", Target: name,
		Details: detail,
	})
	s.forceScheduleReload(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ──────────────────────────────────────────────────────────────────

type scheduleState int

const (
	scheduleAbsent scheduleState = iota
	scheduleGitOnly
	scheduleCronomicon
)

// scheduleSourceState reports whether a name is an editable cronomicon schedule, a
// read-only git schedule, or absent — driving the 404 vs 409 vs proceed branch.
func (s *Server) scheduleSourceState(ctx context.Context, name string) scheduleState {
	var cronomicon, git int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN source='cronomicon' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN source='git'     THEN 1 ELSE 0 END), 0)
		FROM schedules WHERE name=?`, name).Scan(&cronomicon, &git)
	switch {
	case cronomicon > 0:
		return scheduleCronomicon
	case git > 0:
		return scheduleGitOnly
	default:
		return scheduleAbsent
	}
}

type scheduleOwner struct{ source, kind, name string }

// diffOwners returns the owners in all that are absent from live — i.e. the
// recycle-binned referrers a delete proceeded past (FX2-C2).
func diffOwners(all, live []scheduleOwner) []scheduleOwner {
	liveSet := make(map[scheduleOwner]bool, len(live))
	for _, o := range live {
		liveSet[o] = true
	}
	var out []scheduleOwner
	for _, o := range all {
		if !liveSet[o] {
			out = append(out, o)
		}
	}
	return out
}

// scheduleBinding is one ref-expanded definition_schedules row, captured whole
// so a restore can put it back exactly as it was.
//
// It is snapshotted into the delete's tombstone revision because these rows are
// the ONLY record that a job or workflow references this schedule: the referrer
// definitions store their schedule refs nowhere else, so deleting the rows
// without capturing them destroys the binding permanently. A restore that
// cleared deleted_at alone would return a live catalog row that no definition
// fires on any more.
type scheduleBinding struct {
	OwnerSource   string  `json:"ownerSource"`
	OwnerKind     string  `json:"ownerKind"`
	OwnerName     string  `json:"ownerName"`
	Name          string  `json:"name"`
	Cron          string  `json:"cron"`
	Env           *string `json:"env,omitempty"`
	Position      int     `json:"position"`
	SourceRef     string  `json:"sourceRef"`
	StartAt       *string `json:"startAt,omitempty"`
	EndAt         *string `json:"endAt,omitempty"`
	Interval      *string `json:"interval,omitempty"`
	SkipCalendars *string `json:"skipCalendars,omitempty"`
	OnlyCalendars *string `json:"onlyCalendars,omitempty"`
}

// collectScheduleBindings reads every runtime entry expanded from a first-class
// schedule (exact, by source_ref), whole. Drained before return.
func collectScheduleBindings(ctx context.Context, tx *sql.Tx, scheduleName string) ([]scheduleBinding, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT owner_source, owner_kind, owner_name, name, cron, env, position, source_ref,
		       start_at, end_at, interval, skip_calendars, only_calendars
		  FROM definition_schedules WHERE source_ref=?
		 ORDER BY owner_kind, owner_name, position`, scheduleName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []scheduleBinding{}
	for rows.Next() {
		var b scheduleBinding
		var env, startAt, endAt, interval, skipCal, onlyCal sql.NullString
		if err := rows.Scan(&b.OwnerSource, &b.OwnerKind, &b.OwnerName, &b.Name, &b.Cron, &env,
			&b.Position, &b.SourceRef, &startAt, &endAt, &interval, &skipCal, &onlyCal); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			src sql.NullString
			dst **string
		}{{env, &b.Env}, {startAt, &b.StartAt}, {endAt, &b.EndAt}, {interval, &b.Interval},
			{skipCal, &b.SkipCalendars}, {onlyCal, &b.OnlyCalendars}} {
			if p.src.Valid {
				v := p.src.String
				*p.dst = &v
			}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// restoreScheduleBindings replays captured bindings on an un-bin. Entries whose
// owner has since been HARD-deleted (or which the owner has since re-authored)
// are skipped rather than resurrected: INSERT OR IGNORE plus an existence probe
// on the owner keeps a restore from inventing a binding for a definition that is
// no longer there. Returns the owners it touched, for the legacy mirror resync.
//
// FX-A2 — the probe deliberately does NOT filter deleted_at. A binned owner is
// inert, not gone: the scheduler's reload skips binned definitions, so replaying
// its binding fires nothing, and the row is waiting when the owner is restored.
// Filtering it here instead consumed the tombstone and dropped the binding for
// good, because the binding row is the ONLY record that the owner uses this
// schedule and nothing on the owner's own restore path reclaims one. That made
// the outcome depend on the order the operator happened to restore in — schedule
// first silently lost the link, owner first kept it — with nothing telling them
// which they had chosen.
func restoreScheduleBindings(ctx context.Context, tx *sql.Tx, bindings []scheduleBinding) ([]scheduleOwner, error) {
	seen := map[scheduleOwner]bool{}
	var owners []scheduleOwner
	for _, b := range bindings {
		table := "jobs"
		if b.OwnerKind == "workflow" {
			table = "workflows"
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE source=? AND name=?`,
			b.OwnerSource, b.OwnerName).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO definition_schedules
				(owner_source, owner_kind, owner_name, name, cron, env, position, source_ref,
				 start_at, end_at, interval, skip_calendars, only_calendars)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			b.OwnerSource, b.OwnerKind, b.OwnerName, b.Name, b.Cron, strOrNil(b.Env), b.Position,
			b.SourceRef, strOrNil(b.StartAt), strOrNil(b.EndAt), strOrNil(b.Interval),
			strOrNil(b.SkipCalendars), strOrNil(b.OnlyCalendars)); err != nil {
			return nil, err
		}
		o := scheduleOwner{source: b.OwnerSource, kind: b.OwnerKind, name: b.OwnerName}
		if !seen[o] {
			seen[o] = true
			owners = append(owners, o)
		}
	}
	return owners, nil
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// liveScheduleOwners drops owners sitting in the recycle bin (FX-A4). Callers
// that recompute derived state want the FULL set from collectScheduleOwners —
// a binned owner's legacy mirror column still has to be kept honest — so the
// narrowing happens per-consumer rather than in the collector.
//
//nolint:unused // used by deleteScheduleDef's 409 block
func liveScheduleOwners(ctx context.Context, db *sql.DB, owners []scheduleOwner) ([]scheduleOwner, error) {
	var out []scheduleOwner
	for _, o := range owners {
		table := "jobs"
		if o.kind == "workflow" {
			table = "workflows"
		}
		var n int
		//nolint:gosec // table is a package-local constant chosen by the kind switch
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE source=? AND name=? AND deleted_at IS NULL`,
			o.source, o.name).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, o)
		}
	}
	return out, nil
}

// collectScheduleOwners returns the distinct definitions whose schedule entries were
// expanded from a given first-class schedule (exact, by source_ref). Rows are fully
// drained before return (no nested-iterator hold on the pool). Deliberately
// UNFILTERED by deleted_at — see liveScheduleOwners for why its two consumers
// want different sets.
func collectScheduleOwners(ctx context.Context, db *sql.DB, scheduleName string) ([]scheduleOwner, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT owner_source, owner_kind, owner_name FROM definition_schedules
		 WHERE source_ref=? ORDER BY owner_kind, owner_name`, scheduleName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduleOwner
	for rows.Next() {
		var o scheduleOwner
		if err := rows.Scan(&o.source, &o.kind, &o.name); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// recomputeLegacyMirror resets one definition's legacy `schedule` display column to
// its lowest-position remaining schedule entry's cron (NULL when none remain), so the
// denormalized mirror (§3.7) stays consistent after a propagate/cascade. ownerKind is
// CHECK-constrained to job|workflow, so the table name is safe to branch on.
func recomputeLegacyMirror(ctx context.Context, tx *sql.Tx, ownerSource, ownerKind, ownerName string) error {
	var cron sql.NullString
	_ = tx.QueryRowContext(ctx,
		`SELECT cron FROM definition_schedules
		 WHERE owner_source=? AND owner_kind=? AND owner_name=?
		 ORDER BY position LIMIT 1`, ownerSource, ownerKind, ownerName).Scan(&cron)
	table := "jobs"
	if ownerKind == "workflow" {
		table = "workflows"
	}
	var mirror any
	if cron.Valid {
		mirror = cron.String
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET schedule=? WHERE source=? AND name=?`, mirror, ownerSource, ownerName)
	return err
}

// scheduleEnvJSON marshals a schedule env map to its JSON string, or nil when empty
// (matching how sync/compose store definition_schedules.env and schedules.env).
func scheduleEnvJSON(env map[string]string) any {
	if len(env) == 0 {
		return nil
	}
	b, _ := json.Marshal(env)
	return string(b)
}

// writeScheduleDef re-fetches the cronomicon schedule (with its source_ref usedBy
// reverse index) and writes it as the create/edit response.
func (s *Server) writeScheduleDef(w http.ResponseWriter, r *http.Request, name string, status int) {
	row := s.db.QueryRowContext(r.Context(), `
		SELECT name, source, description, cron, env, content_hash, source_path, synced_at, created_at, last_modified_at, tags, start_at, end_at, interval, skip_calendars, only_calendars, uid, 0
		FROM schedules WHERE source='cronomicon' AND name=?`, name)
	sd, err := scanScheduleDef(row)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	sd.UsedBy = []scheduleUsedBy{}
	drows, err := s.db.QueryContext(r.Context(),
		// FX-A4: the FOURTH usedBy site — the create/edit RESPONSE body, which the
		// Schedules UI renders directly. Missing it meant a save reported a binned
		// owner as a user while a GET on the same resource a moment later did not.
		`SELECT owner_kind, owner_name FROM definition_schedules ds
		  WHERE source_ref=?
		    AND NOT EXISTS (SELECT 1 FROM jobs j2 WHERE ds.owner_kind='job'
		                     AND j2.source=ds.owner_source AND j2.name=ds.owner_name
		                     AND j2.deleted_at IS NOT NULL)
		    AND NOT EXISTS (SELECT 1 FROM workflows w2 WHERE ds.owner_kind='workflow'
		                     AND w2.source=ds.owner_source AND w2.name=ds.owner_name
		                     AND w2.deleted_at IS NOT NULL)
		  ORDER BY owner_kind, owner_name`, name)
	if err == nil {
		defer drows.Close()
		for drows.Next() {
			var kind, owner string
			if drows.Scan(&kind, &owner) == nil {
				sd.UsedBy = append(sd.UsedBy, scheduleUsedBy{Name: owner, Kind: kind})
			}
		}
	}
	sd.UsedByCount = len(sd.UsedBy)
	httpx.JSON(w, status, sd)
}
