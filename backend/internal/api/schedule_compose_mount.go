package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
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
	// A schedule built in the app is its owner's (§0a; the owner's answer to
	// question 32): writing one takes compose on that agency, checked in each
	// handler. Until then every one was Global's, and a compose administrator
	// over every agency's alone to write.
	compose := func(h http.HandlerFunc) http.Handler {
		return s.requirePerm(auth.PermCompose, func(p rolePermissions) bool { return p.Compose })(h)
	}
	mux.Handle("POST /api/v1/schedule-defs", compose(s.createScheduleDef))
	mux.Handle("PUT /api/v1/schedule-defs/{name}", compose(s.updateScheduleDef))
	mux.Handle("DELETE /api/v1/schedule-defs/{name}", compose(s.deleteScheduleDef))
}

// requireScheduleOwner answers 403 unless the caller holds compose on the
// agency a schedule is (or is to be) owned by.
func (s *Server) requireScheduleOwner(w http.ResponseWriter, r *http.Request, owner string) bool {
	id, _ := auth.IdentityFrom(r.Context())
	if id.CanAgency(auth.PermCompose, owner) {
		return true
	}
	s.denyEntityAgency(w, r, id, auth.PermCompose, auth.AllScopes,
		"a schedule built in the app belongs to one agency, and writing it takes compose on that agency")
	return false
}

// scheduleInput is the request body for create/update (the ScheduleInput schema).
type scheduleInput struct {
	Name string `json:"name"`
	// OwnerAgency is the agency a new schedule belongs to (§0a, question 32):
	// Global's when empty. Read on create only; a schedule keeps its owner.
	OwnerAgency string            `json:"ownerAgency,omitempty"`
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
	// Whose it is: the agency named, Global's when none is (§0a, question 32);
	// in an agency context, that agency's (2.4.4 S3).
	in.OwnerAgency = strings.TrimSpace(in.OwnerAgency)
	if a, ok := s.ownerInContext(w, r, in.OwnerAgency, "schedule"); !ok {
		return
	} else {
		in.OwnerAgency = a
	}
	if in.OwnerAgency == "" {
		in.OwnerAgency = agencyid.Global
	}
	var known int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, in.OwnerAgency).Scan(&known); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if known == 0 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "ownerAgency names no agency")
		return
	}
	if !s.requireScheduleOwner(w, r, in.OwnerAgency) {
		return
	}
	s.writeComposedSchedule(w, r, in, id.Email, true)
}

func (s *Server) updateScheduleDef(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	ref, ok := s.scheduleRef(w, r, "cronomicon",
		"only cronomicon-source schedules are editable in-app; git schedules round-trip through the publish flow")
	if !ok {
		return
	}
	if !s.requireScheduleOwner(w, r, ref.owner) {
		return
	}
	if deleted, err := s.scheduleBinned(r.Context(), ref.uid); err != nil {
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
	in.Name = ref.name // name and owner are the identity — immutable on edit
	in.OwnerAgency = ref.owner
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
	// Whose it is: the create's ownerAgency, or the owner the route resolved
	// (an edit, a revision restore). Its calendars are that agency's (I2).
	owner := ownerOrGlobal(in.OwnerAgency)
	skipCals, onlyCals, cerr := s.validateCalendarBinding(r.Context(), owner, in.SkipCalendars, in.OnlyCalendars)
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
		// Within the agency (§0a, IN-5; I2): another agency's schedule of the
		// name is nothing to this one.
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*), MAX(deleted_at) FROM schedules WHERE source='cronomicon' AND owner_agency=? AND name=?`,
			owner, in.Name).Scan(&exists, &deleted)
		if exists > 0 {
			msg := "this agency already has an in-app schedule of this name"
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

	// On an edit, WHICH schedule: its uid, resolved once from the owner and
	// name the route carries. Everything below that touches the schedule or
	// the definitions bound to it keys on the uid (migration 1300, GR-8).
	//
	// Owners that reference this schedule (by schedule_uid) — captured before the
	// tx for the legacy-mirror recompute. The owner set is stable across an edit
	// (no rows added/removed; only cron/env change), so a pre-tx read is correct.
	var editUID string
	var owners []scheduleOwner
	if !isCreate {
		editUID = s.inAppScheduleUID(r.Context(), owner, in.Name)
		if editUID == "" {
			httpx.Fail(w, http.StatusNotFound, "not_found", "schedule not found")
			return
		}
		var err error
		owners, err = collectScheduleOwners(r.Context(), s.db, editUID)
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
			                      created_by, created_at, last_modified_by, last_modified_at, uid,
			                      owner_agency)
			VALUES(?, 'cronomicon', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Name, nullStrIf(in.Description), cron, envJSON, hash,
			windowArg(startAt), windowArg(endAt), nullStrIf(interval),
			nullStrIf(calendar.MarshalNames(skipCals)), nullStrIf(calendar.MarshalNames(onlyCals)),
			actor, now, actor, now, db.NewID(),
			// Its owner, as the create named it (§0a, question 32; until then
			// every one was Global's). It has no repository.
			owner); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	} else {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE schedules SET cron=?, env=?, description=?, content_hash=?,
			                     start_at=?, end_at=?, interval=?,
			                     skip_calendars=?, only_calendars=?,
			                     last_modified_by=?, last_modified_at=?
			WHERE uid=?`,
			cron, envJSON, nullStrIf(in.Description), hash,
			windowArg(startAt), windowArg(endAt), nullStrIf(interval),
			nullStrIf(calendar.MarshalNames(skipCals)), nullStrIf(calendar.MarshalNames(onlyCals)),
			actor, now, editUID); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// Propagate to every referencing job/workflow (D1c), by schedule_uid: the
		// entries expanded from THIS schedule. It went by source_ref, the bare
		// name, until 1300, and so rewrote the timing of the definitions bound to
		// any other schedule of that name as well: Git's `nightly`, when the
		// in-app `nightly` was edited (H12). An inline entry that coincidentally
		// shares the name has no schedule_uid and is untouched, as before.
		// The activation window rides along with cron/env (AW-4): a ref expansion is
		// a copy of the catalog row, so an edited window must reach the runtime
		// entries or the catalog and the scheduler would disagree.
		//
		// Only the definitions of the schedule's own agency (§0a, IN-8; the
		// owner's answer to question 32): another agency's definition bound to
		// it before §0a keeps the timing it copied, and the notice
		// global_schedule_bound says so.
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE definition_schedules SET cron=?, env=?, start_at=?, end_at=?, interval=?,
			                                 skip_calendars=?, only_calendars=? WHERE schedule_uid=?
			    AND `+agencyid.DefinitionOf("definition_schedules.owner_uid", "definition_schedules.owner_kind")+`
			      = (SELECT owner_agency FROM schedules WHERE uid = ?)`,
			cron, envJSON, windowArg(startAt), windowArg(endAt), nullStrIf(interval),
			nullStrIf(calendar.MarshalNames(skipCals)), nullStrIf(calendar.MarshalNames(onlyCals)), editUID, editUID); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// Keep the legacy display mirror (jobs/workflows.schedule = lowest-position
		// cron, §3.7) consistent for owners whose position-0 entry is this schedule.
		for _, o := range owners {
			if err := recomputeLegacyMirror(r.Context(), tx, o); err != nil {
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
	_ = tx.QueryRowContext(r.Context(),
		`SELECT uid FROM schedules WHERE source='cronomicon' AND owner_agency=? AND name=?`, owner, in.Name).Scan(&schedUID)
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
	_ = workflow.InsertChangeLog(r.Context(), s.db, owner, actor, "Schedules", auditAction, in.Name, "Cronomicon schedule "+strings.ToLower(auditAction))
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		AgencyID: owner, Kind: "config", Actor: actor, Category: "Schedules", Action: auditAction, Target: in.Name,
	})
	// A bare create references no definition_schedules, so a reload is a cheap
	// no-op — but harmless, and keeps the seam consistent with edit/delete (D8).
	s.forceScheduleReload(r.Context())
	s.writeScheduleDef(w, r, owner, in.Name, status)
}

func (s *Server) deleteScheduleDef(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	ref, ok := s.scheduleRef(w, r, "cronomicon", "only cronomicon-source schedules are deletable in-app")
	if !ok {
		return
	}
	if !s.requireScheduleOwner(w, r, ref.owner) {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	// WHICH schedule: its uid, resolved once from the owner and name the route
	// carries. The referrers, the soft delete, the tombstone and the detach
	// below all key on it (migration 1300, GR-8).
	name, schedUID := ref.name, ref.uid

	// Capture referrers BEFORE deletion: needed for both the block-by-default 409
	// and (when forced) the legacy-mirror recompute after the cascade detaches them.
	owners, err := collectScheduleOwners(r.Context(), s.db, schedUID)
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
		`UPDATE schedules SET deleted_at=?, deleted_by=? WHERE uid=? AND deleted_at IS NULL`,
		now, id.Email, schedUID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "this schedule is already in the recycle bin")
		return
	}
	// Cascade the ref-expanded runtime entries (exact, by schedule_uid — never an
	// inline entry of the same name, and never an entry expanded from ANOTHER
	// schedule of the same name, which the bare source_ref used to take with it:
	// force-deleting the in-app `nightly` removed the timing of every definition
	// bound to Git's, H12). These rows ARE the referrers' record that
	// they reference this schedule — nothing else stores it — so they are read
	// into the tombstone below before being removed, and restore replays them.
	detached, err := collectScheduleBindings(r.Context(), tx, schedUID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM definition_schedules WHERE schedule_uid=?`, schedUID); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// Resync the legacy display column for each detached owner to its next remaining
	// schedule cron (or NULL), so the UI shows no stale anomaly (D3 Legacy Column Sync).
	for _, o := range owners {
		if err := recomputeLegacyMirror(r.Context(), tx, o); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}
	if err := snapshotRevision(r.Context(), tx, revKindSchedule, "cronomicon", name, schedUID, id.Email, revActionDeleted,
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
	agency := s.definitionAgency(r.Context(), "schedule", schedUID) // the schedule's (C1)
	_ = workflow.InsertChangeLog(r.Context(), s.db, agency, id.Email, "Schedules", "Deleted", name, detail)
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		AgencyID: agency, Kind: "config", Actor: id.Email, Category: "Schedules", Action: "Deleted", Target: name,
		Details: detail,
	})
	s.forceScheduleReload(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// scheduleRefOf is the schedule a {name} route means: its owner, name and uid.
type scheduleRefOf struct{ owner, name, uid string }

// scheduleRef resolves the schedule of a source a {name} route means (§0a,
// IN-5; I2: a name is unique within an agency only, so the route says whose):
// the name in the agency ?agencyId= names, or the one agency that has a
// schedule of the source and name. 409 ambiguous_schedule asks for the agency
// when more than one has. When none has, 404 — or, for the in-app source with
// gitOnly given, 409 conflict with that message when a Git schedule holds the
// name (an in-app write cannot touch one; it round-trips through its
// repository). Live before binned: a name one agency holds live and another
// holds in its recycle bin means the live one (the binned one is reachable
// with ?agencyId= and from the recycle bin); the callers decide what a binned
// schedule may have done to it.
func (s *Server) scheduleRef(w http.ResponseWriter, r *http.Request, source, gitOnly string) (scheduleRefOf, bool) {
	name := r.PathValue("name")
	agency := strings.TrimSpace(r.URL.Query().Get("agencyId"))
	q := `SELECT owner_agency, uid, deleted_at IS NULL FROM schedules WHERE source = ? AND name = ?`
	args := []any{source, name}
	if agency != "" {
		q += ` AND owner_agency = ?`
		args = append(args, agency)
	}
	rows, err := s.db.QueryContext(r.Context(), q+` ORDER BY owner_agency`, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return scheduleRefOf{}, false
	}
	defer rows.Close()
	var found, live []scheduleRefOf
	for rows.Next() {
		var owner string
		var uid sql.NullString
		var isLive int
		// Only the agencies the caller reads (2.5.0 C2, CF-1): another agency's
		// schedule of the name is neither found nor counted (question 72: its
		// detail, env and usedBy were every session's, and "ambiguous" said
		// another agency had one).
		if rows.Scan(&owner, &uid, &isLive) == nil && readsAgency(r, owner) {
			ref := scheduleRefOf{owner: owner, name: name, uid: uid.String}
			found = append(found, ref)
			if isLive == 1 {
				live = append(live, ref)
			}
		}
	}
	if len(live) > 0 {
		found = live
	}
	switch len(found) {
	case 1:
		return found[0], true
	case 0:
		if gitOnly != "" && source == "cronomicon" {
			gq, gargs := `SELECT COUNT(*) FROM schedules WHERE source = 'git' AND name = ?`, []any{name}
			if agency != "" {
				gq, gargs = gq+` AND owner_agency = ?`, append(gargs, agency)
			}
			var git int
			_ = s.db.QueryRowContext(r.Context(), gq, gargs...).Scan(&git)
			if git > 0 {
				httpx.Fail(w, http.StatusConflict, "conflict", gitOnly)
				return scheduleRefOf{}, false
			}
		}
		httpx.Fail(w, http.StatusNotFound, "not_found", "schedule not found")
		return scheduleRefOf{}, false
	}
	httpx.Fail(w, http.StatusConflict, "ambiguous_schedule",
		"more than one agency has a schedule of this name; say whose with ?agencyId=")
	return scheduleRefOf{}, false
}

// inAppScheduleUID resolves an agency's in-app schedule of a name to its uid,
// "" when there is none. Binned or live, as scheduleRef.
func (s *Server) inAppScheduleUID(ctx context.Context, owner, name string) string {
	var uid sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT uid FROM schedules WHERE source='cronomicon' AND owner_agency=? AND name=?`, owner, name).Scan(&uid)
	return uid.String
}

// scheduleBinned reports whether the schedule of a uid is in the recycle bin.
func (s *Server) scheduleBinned(ctx context.Context, uid string) (bool, error) {
	var deletedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT deleted_at FROM schedules WHERE uid = ?`, uid).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return deletedAt.Valid, err
}

// scheduleOwner is one definition bound to a schedule: its (source, kind, name)
// and its uid, "" for an entry from before 1050 that records none. The uid is
// what says WHICH definition once two agencies hold the name (2.5.0 C12, 99).
type scheduleOwner struct{ source, kind, name, uid string }

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
	OwnerSource string `json:"ownerSource"`
	OwnerKind   string `json:"ownerKind"`
	OwnerName   string `json:"ownerName"`
	// OwnerUID is WHICH definition (migration 1300). The name alone does not say:
	// two agencies may each own an in-app job of one name, and a restore that
	// went by the name could not tell whose binding this was. Absent from a
	// tombstone written before 2.4.0; see restoreScheduleBindings for what a
	// restore does then.
	OwnerUID      string  `json:"ownerUid,omitempty"`
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
// schedule (exact, by schedule_uid), whole. Drained before return.
func collectScheduleBindings(ctx context.Context, tx *sql.Tx, scheduleUID string) ([]scheduleBinding, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT owner_source, owner_kind, owner_name, name, cron, env, position, COALESCE(source_ref, ''),
		       start_at, end_at, interval, skip_calendars, only_calendars, COALESCE(owner_uid, '')
		  FROM definition_schedules WHERE schedule_uid=?
		 ORDER BY owner_kind, owner_name, position`, scheduleUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []scheduleBinding{}
	for rows.Next() {
		var b scheduleBinding
		var env, startAt, endAt, interval, skipCal, onlyCal sql.NullString
		if err := rows.Scan(&b.OwnerSource, &b.OwnerKind, &b.OwnerName, &b.Name, &b.Cron, &env,
			&b.Position, &b.SourceRef, &startAt, &endAt, &interval, &skipCal, &onlyCal, &b.OwnerUID); err != nil {
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
//
// A restored binding is a binding like any other (migration 1300): it carries
// its owner's uid and the schedule's. It used to carry neither. Nothing that
// keys on a uid saw such a row, so the next save of the owning definition did
// not replace it and added a second entry of the same name, and "OR IGNORE"
// ignored nothing, because a row with no owner uid is outside the unique index.
// scheduleUID is the schedule being restored. The owner is the one the
// tombstone names by uid (scheduleBinding.OwnerUID, captured at the delete
// since 2.4.0), so that two same-named definitions each get their own binding
// back. A tombstone written before 2.4.0 has the owner's name only: the uid is
// taken when the name is exactly one definition, and when it is two the
// binding is put back under the name as it always was. It is never skipped
// for being ambiguous: a skipped binding is a job that silently stops firing.
func restoreScheduleBindings(ctx context.Context, tx *sql.Tx, bindings []scheduleBinding, scheduleUID string) ([]scheduleOwner, error) {
	seen := map[scheduleOwner]bool{}
	var owners []scheduleOwner
	for _, b := range bindings {
		table := "jobs"
		if b.OwnerKind == "workflow" {
			table = "workflows"
		}
		// Which definition the binding goes back to.
		ownerUID := b.OwnerUID
		if ownerUID != "" {
			// The tombstone says: the definition with this uid, if it is still
			// there (binned counts, FX-A2; hard-deleted does not).
			var n int
			//nolint:gosec // table is a package-local constant chosen by the kind switch
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE uid=?`, ownerUID).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				continue
			}
		} else {
			// A tombstone written before 2.4.0 names its owner by NAME only.
			var n int
			var only sql.NullString
			//nolint:gosec // table is a package-local constant chosen by the kind switch
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*), MAX(uid) FROM `+table+` WHERE source=? AND name=?`,
				b.OwnerSource, b.OwnerName).Scan(&n, &only); err != nil {
				return nil, err
			}
			switch {
			case n == 0:
				continue
			case n == 1:
				ownerUID = only.String
			default:
				// Two definitions hold the name and the tombstone cannot say
				// which was meant. The binding is put back as it always was,
				// under the name and with no owner uid, so that nothing stops
				// firing because of an upgrade: skipping it would silently
				// take the timing away from a job that had it. It is the one
				// case a restore still leaves a row without an owner uid, and
				// it is not written twice.
				var have int
				if err := tx.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM definition_schedules
					 WHERE owner_uid IS NULL AND owner_source=? AND owner_kind=? AND owner_name=? AND name=?`,
					b.OwnerSource, b.OwnerKind, b.OwnerName, b.Name).Scan(&have); err != nil {
					return nil, err
				}
				if have > 0 {
					continue
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO definition_schedules
				(owner_source, owner_kind, owner_name, name, cron, env, position, source_ref,
				 start_at, end_at, interval, skip_calendars, only_calendars, owner_uid, schedule_uid)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,NULLIF(?, ''),?)`,
			b.OwnerSource, b.OwnerKind, b.OwnerName, b.Name, b.Cron, strOrNil(b.Env), b.Position,
			b.SourceRef, strOrNil(b.StartAt), strOrNil(b.EndAt), strOrNil(b.Interval),
			strOrNil(b.SkipCalendars), strOrNil(b.OnlyCalendars), ownerUID, scheduleUID); err != nil {
			return nil, err
		}
		o := scheduleOwner{source: b.OwnerSource, kind: b.OwnerKind, name: b.OwnerName, uid: ownerUID}
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
// expanded from a given first-class schedule (exact, by schedule_uid). Rows are fully
// drained before return (no nested-iterator hold on the pool). Deliberately
// UNFILTERED by deleted_at — see liveScheduleOwners for why its two consumers
// want different sets.
func collectScheduleOwners(ctx context.Context, db *sql.DB, scheduleUID string) ([]scheduleOwner, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT owner_source, owner_kind, owner_name, COALESCE(owner_uid, '') FROM definition_schedules
		 WHERE schedule_uid=? ORDER BY owner_kind, owner_name`, scheduleUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduleOwner
	for rows.Next() {
		var o scheduleOwner
		if err := rows.Scan(&o.source, &o.kind, &o.name, &o.uid); err != nil {
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
//
// The definition is found by its uid: until 2.5.0 C12 both the read and the
// write went by (source, name), so an edit of FIN's `nightly` rewrote the
// column on TAX's `backup` too, from whichever agency's entry came first
// (present defect 99, question 119). An owner that records no uid (an entry
// from before 1050) is still found by its name.
func recomputeLegacyMirror(ctx context.Context, tx *sql.Tx, o scheduleOwner) error {
	ownerSource, ownerKind, ownerName := o.source, o.kind, o.name
	table := "jobs"
	if ownerKind == "workflow" {
		table = "workflows"
	}
	if o.uid != "" {
		var cron sql.NullString
		_ = tx.QueryRowContext(ctx,
			`SELECT cron FROM definition_schedules WHERE owner_kind=? AND owner_uid=?
			 ORDER BY position LIMIT 1`, ownerKind, o.uid).Scan(&cron)
		var mirror any
		if cron.Valid {
			mirror = cron.String
		}
		//nolint:gosec // table is a package-local constant chosen by the kind switch
		_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET schedule=? WHERE uid=?`, mirror, o.uid)
		return err
	}
	var cron sql.NullString
	_ = tx.QueryRowContext(ctx,
		`SELECT cron FROM definition_schedules
		 WHERE owner_source=? AND owner_kind=? AND owner_name=?
		 ORDER BY position LIMIT 1`, ownerSource, ownerKind, ownerName).Scan(&cron)
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
func (s *Server) writeScheduleDef(w http.ResponseWriter, r *http.Request, owner, name string, status int) {
	row := s.db.QueryRowContext(r.Context(), `
		SELECT name, source, description, cron, env, content_hash, source_path, synced_at, created_at, last_modified_at, tags, start_at, end_at, interval, skip_calendars, only_calendars, uid, 0, owner_agency
		FROM schedules WHERE source='cronomicon' AND owner_agency=? AND name=?`, owner, name)
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
		  WHERE schedule_uid=?
		    AND NOT EXISTS (SELECT 1 FROM jobs j2 WHERE ds.owner_kind='job'
		                     AND j2.source=ds.owner_source AND j2.name=ds.owner_name
		                     AND j2.deleted_at IS NOT NULL)
		    AND NOT EXISTS (SELECT 1 FROM workflows w2 WHERE ds.owner_kind='workflow'
		                     AND w2.source=ds.owner_source AND w2.name=ds.owner_name
		                     AND w2.deleted_at IS NOT NULL)
		  ORDER BY owner_kind, owner_name`, sd.UID)
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

// ownerOrGlobal is an owner as given, Global's when none is.
func ownerOrGlobal(owner string) string {
	if strings.TrimSpace(owner) == "" {
		return agencyid.Global
	}
	return owner
}
