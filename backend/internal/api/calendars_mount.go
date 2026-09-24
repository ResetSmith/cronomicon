package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountCalendars owns the working-calendar authoring surface
// (the calendar-update plan, CAL-4 — Phase 1B).
//
// A calendar is a named set of wall-clock dates that schedule entries bind to in
// two polarities (skip / only); Phase 1A built the enforcement, this is what
// makes it authorable by something other than SQL.
//
// # Why writes are admin-only
//
// The write routes sit behind requireCompose, the SAME gate as schedule-defs and
// for the same class of reason: someone who can edit a calendar can lift a change
// freeze, or — by adding a global calendar covering today — halt every scheduled
// fire in the installation. In `only` mode the failure is silent, which is worse:
// an entry whose run-day calendar is emptied simply never fires again and nothing
// in the UI says why.
//
// 🔴 Read the RB-30 note at schedule_compose_mount.go:39 before widening this.
//
// Reads are session-only, not admin: the Inventory, Upcoming and (Phase 1C)
// Calendars views all need them, and a calendar carries no secret material —
// it is a list of dates.
//
//	GET    /api/v1/calendars              list, with coverage summary (CAL-16)
//	GET    /api/v1/calendars/{name}       one calendar with its days
//	POST   /api/v1/calendars              create
//	PUT    /api/v1/calendars/{name}       update metadata/flags (not days)
//	DELETE /api/v1/calendars/{name}       409 when bound unless ?force=true
//	PUT    /api/v1/calendars/{name}/days  bulk replace the day list
func (s *Server) mountCalendars(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	mux.Handle("GET /api/v1/calendars", requireSession(http.HandlerFunc(s.listCalendars)))
	mux.Handle("GET /api/v1/calendars/{name}", requireSession(http.HandlerFunc(s.getCalendar)))
	mux.Handle("POST /api/v1/calendars", s.requireComposeAdmin(http.HandlerFunc(s.createCalendar)))
	mux.Handle("PUT /api/v1/calendars/{name}", s.requireComposeAdmin(http.HandlerFunc(s.updateCalendar)))
	mux.Handle("DELETE /api/v1/calendars/{name}", s.requireComposeAdmin(http.HandlerFunc(s.deleteCalendar)))
	mux.Handle("PUT /api/v1/calendars/{name}/days", s.requireComposeAdmin(http.HandlerFunc(s.replaceCalendarDays)))
}

// calendarInput is the create/update request body (the CalendarInput schema).
type calendarInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Global unions this calendar's days into every entry's skip set (§2.8).
	// Skip-polarity only — see validateGlobalUsable.
	Global bool `json:"global"`
	// RecordSuppressed opts only-mode suppressions into History (CAL-Q4).
	RecordSuppressed bool `json:"recordSuppressed"`
	// Days is accepted on CREATE as a convenience so a calendar and its dates
	// arrive in one call; PUT ignores it (use PUT .../days) so a metadata edit
	// cannot silently wipe a year of dates by omitting the field.
	Days []calendarDayInput `json:"days,omitempty"`
}

type calendarDayInput struct {
	Day   string `json:"day"`
	Label string `json:"label,omitempty"`
}

// calendarRow is the JSON shape of the Calendar schema.
type calendarRow struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	Description      string `json:"description,omitempty"`
	Global           bool   `json:"global"`
	RecordSuppressed bool   `json:"recordSuppressed"`
	DayCount         int    `json:"dayCount"`
	// LastDay is the newest date in the calendar, and DaysRemaining how far that
	// is from today — the two fields CAL-16's expiry warning is computed from.
	// Both null for an empty calendar. DaysRemaining goes NEGATIVE once the last
	// day has passed, which is the state that matters: a skip calendar that ran
	// out of days silently stops suppressing.
	LastDay       *string            `json:"lastDay"`
	DaysRemaining *int               `json:"daysRemaining"`
	UsedBy        []calendarBinding  `json:"usedBy,omitempty"`
	Days          []calendarDayInput `json:"days,omitempty"`
	CreatedBy     string             `json:"createdBy,omitempty"`
	CreatedAt     string             `json:"createdAt,omitempty"`
}

// calendarBinding is one schedule entry that names a calendar.
type calendarBinding struct {
	OwnerKind    string `json:"ownerKind"`
	OwnerName    string `json:"ownerName"`
	OwnerSource  string `json:"ownerSource"`
	ScheduleName string `json:"scheduleName"`
	Polarity     string `json:"polarity"` // skip | only
	// BinnedOwner marks a binding whose owner sits in the recycle bin (FX2-C1).
	// Only the delete guard sees these (allCalendarBindings); every JSON surface
	// serves the live-only set, so the field never reaches a response.
	BinnedOwner bool `json:"-"`
	// R2F-3 — the owner's identity and (for job owners) derived agencies, so a
	// bindings list can qualify an owner name two departments share. Additive.
	OwnerUID      string   `json:"ownerUid,omitempty"`
	OwnerAgencies []string `json:"ownerAgencies,omitempty"`
}

// calendarExpiryWarningDays is CAL-16's threshold: warn while there is still
// time to renew rather than at exhaustion. With no shipped holiday content
// (CAL-Q8) an unrenewed calendar is this feature's likeliest failure and this
// number is the only thing standing between it and a silent policy violation.
const calendarExpiryWarningDays = 60

func (s *Server) listCalendars(w http.ResponseWriter, r *http.Request) {
	rows, err := s.loadCalendarRows(r.Context(), "")
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	bindings, err := collectCalendarBindings(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	items := make([]calendarRow, 0, len(rows))
	for _, c := range rows {
		c.UsedBy = bindings[c.Name]
		c.Days = nil // the list view carries counts and coverage, not every date
		items = append(items, c)
	}
	// The threshold ships with the data so the badge in the UI and any future
	// server-side warning cannot drift to two different numbers.
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items":             items,
		"expiryWarningDays": calendarExpiryWarningDays,
	})
}

func (s *Server) getCalendar(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rows, err := s.loadCalendarRows(r.Context(), name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if len(rows) == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "calendar not found")
		return
	}
	c := rows[0]
	bindings, err := collectCalendarBindings(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	c.UsedBy = bindings[c.Name]
	httpx.JSON(w, http.StatusOK, c)
}

func (s *Server) createCalendar(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in calendarInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !composeNameRe.MatchString(in.Name) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid calendar name (want "+composeNameRe.String()+")")
		return
	}
	days, verr := normalizeCalendarDays(in.Days)
	if verr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", verr)
		return
	}
	var exists int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM calendars WHERE name=?`, in.Name).Scan(&exists)
	if exists > 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "a calendar with this name already exists")
		return
	}
	// CAL-27's first refusal applies on CREATE too, not only on update: a
	// force-deleted calendar leaves its `only` bindings dangling by design, so
	// re-creating that same name as global is a reachable path to the very state
	// the refusal exists to prevent.
	if in.Global {
		if verr, err := s.calendarUsedAsOnly(r.Context(), in.Name); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		} else if verr != "" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", verr)
			return
		}
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO calendars(source, name, description, global, record_suppressed,
		                      created_by, created_at, last_modified_by, last_modified_at)
		VALUES('amadeus', ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Name, nullStrIf(in.Description), boolInt(in.Global), boolInt(in.RecordSuppressed),
		id.Email, now, id.Email, now); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := writeCalendarDays(r.Context(), tx, in.Name, days); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.auditCalendar(r, id.Email, "Created", in.Name, "Calendar created with "+strconv.Itoa(len(days))+" day(s)")
	s.writeCalendar(w, r, in.Name, http.StatusCreated)
}

func (s *Server) updateCalendar(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	name := r.PathValue("name")
	var exists int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM calendars WHERE name=?`, name).Scan(&exists)
	if exists == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "calendar not found")
		return
	}
	var in calendarInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	// CAL-27 — a calendar bound into any entry's `only` role may not be made
	// global. The two are incompatible by construction: global means "unioned
	// into every skip set", and a calendar that is simultaneously somebody's
	// run-day list would then both permit and forbid the same day.
	if in.Global {
		if verr, err := s.calendarUsedAsOnly(r.Context(), name); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		} else if verr != "" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", verr)
			return
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE calendars SET description=?, global=?, record_suppressed=?,
		                     last_modified_by=?, last_modified_at=?
		 WHERE name=?`,
		nullStrIf(in.Description), boolInt(in.Global), boolInt(in.RecordSuppressed),
		id.Email, now, name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.auditCalendar(r, id.Email, "Updated", name, "Calendar updated")
	s.writeCalendar(w, r, name, http.StatusOK)
}

// replaceCalendarDays is the bulk day-replacement route. Days are replaced
// wholesale rather than patched: the authoring gesture is "here is next year's
// list", and a per-day diff API would make an eleven-row edit eleven calls.
func (s *Server) replaceCalendarDays(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	name := r.PathValue("name")
	var exists int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM calendars WHERE name=?`, name).Scan(&exists)
	if exists == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "calendar not found")
		return
	}
	var body struct {
		Days []calendarDayInput `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	days, verr := normalizeCalendarDays(body.Days)
	if verr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", verr)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM calendar_days WHERE calendar_name=?`, name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := writeCalendarDays(r.Context(), tx, name, days); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE calendars SET last_modified_by=?, last_modified_at=? WHERE name=?`,
		id.Email, time.Now().UTC().Format(time.RFC3339), name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.auditCalendar(r, id.Email, "Updated", name, "Calendar days replaced ("+strconv.Itoa(len(days))+" day(s))")
	s.writeCalendar(w, r, name, http.StatusOK)
}

// deleteCalendar removes a calendar and (by cascade) its days.
//
// 409 when any schedule entry still names it, unless ?force=true — mirroring
// deleteScheduleDef. Forcing leaves dangling names, whose behaviour §2.2 defines
// by set arithmetic: a dangling skip binding fires (visible, correctable), a
// dangling only binding never fires (silent). That asymmetry is exactly why the
// 409 is the default and force has to be asked for.
func (s *Server) deleteCalendar(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	name := r.PathValue("name")
	var exists int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM calendars WHERE name=?`, name).Scan(&exists)
	if exists == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "calendar not found")
		return
	}
	// FX2-C1 — the guard counts BINNED owners' bindings too, unlike every read
	// surface. This delete is HARD while a binned owner is recoverable: forcing
	// past a binned binding strips a restored job's holiday protection with no
	// signal, so the operator must be told what force would detach — annotated,
	// so "in recycle bin" reads as dormant rather than live.
	bindings, err := allCalendarBindings(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	used := bindings[name]
	if len(used) > 0 && r.URL.Query().Get("force") != "true" {
		refs := make([]string, 0, len(used))
		for _, b := range used {
			ref := b.Polarity + ":" + b.OwnerKind + ":" + b.OwnerName + "/" + b.ScheduleName
			if b.BinnedOwner {
				ref += " (in recycle bin)"
			}
			refs = append(refs, ref)
		}
		httpx.Fail(w, http.StatusConflict, "conflict",
			"calendar is bound by "+strconv.Itoa(len(used))+" schedule entry/entries: "+strings.Join(refs, ", ")+
				" — pass ?force=true to delete and leave those bindings dangling")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM calendars WHERE name=?`, name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.auditCalendar(r, id.Email, "Deleted", name, "Calendar deleted")
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ────────────────────────────────────────────────────────────────

// loadCalendarRows reads calendars (all, or one by name) with their days and the
// derived coverage fields. One query for the calendars, one for the days — the
// day rows are drained fully before anything else runs, since a nested query on
// this pool with an iterator still open is the documented deadlock.
func (s *Server) loadCalendarRows(ctx context.Context, only string) ([]calendarRow, error) {
	q := `SELECT source, name, COALESCE(description,''), global, record_suppressed,
	             COALESCE(created_by,''), COALESCE(created_at,'')
	        FROM calendars`
	var args []any
	if only != "" {
		q += ` WHERE name = ?`
		args = append(args, only)
	}
	q += ` ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	byName := map[string]*calendarRow{}
	var out []calendarRow
	for rows.Next() {
		var c calendarRow
		var global, record int
		if err := rows.Scan(&c.Source, &c.Name, &c.Description, &global, &record, &c.CreatedBy, &c.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		c.Global = global == 1
		c.RecordSuppressed = record == 1
		c.Days = []calendarDayInput{}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		byName[out[i].Name] = &out[i]
	}
	if len(out) == 0 {
		return out, nil
	}

	dq := `SELECT calendar_name, day, COALESCE(label,'') FROM calendar_days`
	var dargs []any
	if only != "" {
		dq += ` WHERE calendar_name = ?`
		dargs = append(dargs, only)
	}
	dq += ` ORDER BY calendar_name, day`
	drows, err := s.db.QueryContext(ctx, dq, dargs...)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var cname, day, label string
		if err := drows.Scan(&cname, &day, &label); err != nil {
			return nil, err
		}
		if c, ok := byName[cname]; ok {
			c.Days = append(c.Days, calendarDayInput{Day: day, Label: label})
		}
	}
	if err := drows.Err(); err != nil {
		return nil, err
	}

	// CAL-16's coverage summary, derived rather than stored so it cannot go stale.
	todayStr := calendar.DayOf(time.Now(), s.appLocation())
	for i := range out {
		c := &out[i]
		c.DayCount = len(c.Days)
		if c.DayCount == 0 {
			continue
		}
		last := c.Days[len(c.Days)-1].Day // ORDER BY day ⇒ the newest is last
		lastCopy := last
		c.LastDay = &lastCopy
		if n, ok := daysBetween(todayStr, last); ok {
			nn := n
			c.DaysRemaining = &nn
		}
	}
	return out, nil
}

// daysBetween returns whole days from `from` to `to`, negative when `to` is in
// the past. Both are 'YYYY-MM-DD'; the arithmetic is done in UTC on parsed dates
// so it measures calendar days rather than elapsed hours.
func daysBetween(from, to string) (int, bool) {
	f, err := time.Parse(calendar.DayFormat, from)
	if err != nil {
		return 0, false
	}
	t, err := time.Parse(calendar.DayFormat, to)
	if err != nil {
		return 0, false
	}
	return int(t.Sub(f).Hours() / 24), true
}

// collectCalendarBindings maps calendar name → the LIVE schedule entries naming
// it, in one pass over definition_schedules. Used by the list/get responses and
// CAL-16's dangling-reference detection.
//
// FX-A4: entries owned by a binned definition are excluded — they cannot fire,
// so reporting them as bindings showed dangling references an operator could
// not act on. The DELETE guard is the one consumer that must see them
// (FX2-C1), and calls allCalendarBindings instead.
func collectCalendarBindings(ctx context.Context, database *sql.DB) (map[string][]calendarBinding, error) {
	all, err := allCalendarBindings(ctx, database)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]calendarBinding, len(all))
	for name, bs := range all {
		for _, b := range bs {
			if !b.BinnedOwner {
				out[name] = append(out[name], b)
			}
		}
	}
	return out, nil
}

// allCalendarBindings is collectCalendarBindings WITHOUT the live-owner filter:
// bindings whose owner is in the recycle bin are included and flagged (FX2-C1).
//
// The delete guard needs them because a calendar delete is HARD — no bin, no
// tombstone — while a binned owner is fully recoverable: deleting the calendar
// out from under it leaves the restored definition's entry naming a calendar
// that no longer exists, which evaluates as no suppression, and the job
// silently fires on every holiday it was configured to skip. FX2-Q1 split the
// old blanket ruling (FX-Q9) over exactly this asymmetry.
func allCalendarBindings(ctx context.Context, database *sql.DB) (map[string][]calendarBinding, error) {
	out := map[string][]calendarBinding{}
	addFlag := func(kind, owner, src, sched string, names []string, polarity string, binned bool, uid string, agencies []string) {
		for _, n := range names {
			out[n] = append(out[n], calendarBinding{
				OwnerKind: kind, OwnerName: owner, OwnerSource: src,
				ScheduleName: sched, Polarity: polarity, BinnedOwner: binned,
				OwnerUID: uid, OwnerAgencies: agencies,
			})
		}
	}
	// A first-class schedule is its OWN owner — it has no job/workflow identity to
	// carry, so these bindings never badge (nothing to tell apart).
	add := func(kind, owner, src, sched string, names []string, polarity string) {
		addFlag(kind, owner, src, sched, names, polarity, false, "", nil)
	}

	// Runtime entries — what actually fires (or would, once a binned owner is
	// restored). Binned-ness is computed as a flag rather than a WHERE filter so
	// the live and delete-guard views cannot drift: one query, and the one rule
	// is the shared binnedOwnerSQL fragment (FX2-F2).
	rows, err := database.QueryContext(ctx, `
		SELECT owner_source, owner_kind, owner_name, name,
		       COALESCE(skip_calendars,''), COALESCE(only_calendars,''),
		       `+binnedOwnerSQL+`,
		       -- R2F-3: the owner's identity + derived agencies, in the same
		       -- select rather than a per-binding lookup.
		       COALESCE(ds.owner_uid,''),
		       (SELECT GROUP_CONCAT(a.name, '\x1f')
		          FROM jobs j
		          JOIN scopes sc         ON sc.name = j.scope
		          JOIN scope_agencies sa ON sa.scope_id = sc.id
		          JOIN agencies a        ON a.id = sa.agency_id
		         WHERE ds.owner_kind = 'job' AND j.source = ds.owner_source AND j.name = ds.owner_name)
		  FROM definition_schedules ds
		 WHERE (skip_calendars IS NOT NULL OR only_calendars IS NOT NULL)
		 ORDER BY owner_source, owner_kind, owner_name, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, kind, owner, sched, skipRaw, onlyRaw, ownerUID string
		var binned bool
		var agencies sql.NullString
		if err := rows.Scan(&src, &kind, &owner, &sched, &skipRaw, &onlyRaw, &binned, &ownerUID, &agencies); err != nil {
			rows.Close()
			return nil, err
		}
		ag := splitAgencies(agencies)
		addFlag(kind, owner, src, sched, calendar.ParseNames(skipRaw), "skip", binned, ownerUID, ag)
		addFlag(kind, owner, src, sched, calendar.ParseNames(onlyRaw), "only", binned, ownerUID, ag)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// FIRST-CLASS schedules — a reusable policy object that no job references YET
	// is still a binding. Omitting these would let a calendar be deleted (or
	// promoted to global) out from under a schedule that names it, and the damage
	// would only appear when somebody bound the ref to a job.
	srows, err := database.QueryContext(ctx, `
		SELECT source, name, COALESCE(skip_calendars,''), COALESCE(only_calendars,'')
		  FROM schedules
		 WHERE skip_calendars IS NOT NULL OR only_calendars IS NOT NULL
		 ORDER BY source, name`)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var src, name, skipRaw, onlyRaw string
		if err := srows.Scan(&src, &name, &skipRaw, &onlyRaw); err != nil {
			return nil, err
		}
		add("schedule", name, src, name, calendar.ParseNames(skipRaw), "skip")
		add("schedule", name, src, name, calendar.ParseNames(onlyRaw), "only")
	}
	return out, srows.Err()
}

// calendarUsedAsOnly returns a validation message if the named calendar is bound
// into any entry's `only` role (CAL-27's first refusal).
//
// FX2-C1 — this is a VALIDATION guard, not a runtime gate, so like the delete
// guard it judges on ALL bindings including recycle-binned owners. The state it
// refuses is unfireable by construction: a global calendar is unioned into every
// entry's skip set, so an entry that also names it as `only` has every day
// either vetoed by the global tier or suppressed by the only-rule — the job
// never fires again. Judging on live bindings alone let an operator reach that
// state by binning the sole referrer first, and the damage appeared silently on
// its restore. Binned bindings are annotated so the refusal is actionable.
func (s *Server) calendarUsedAsOnly(ctx context.Context, name string) (string, error) {
	bindings, err := allCalendarBindings(ctx, s.db)
	if err != nil {
		return "", err
	}
	for _, b := range bindings[name] {
		if b.Polarity == "only" {
			where := b.OwnerKind + ":" + b.OwnerName + "/" + b.ScheduleName
			if b.BinnedOwner {
				where += " (in recycle bin)"
			}
			return "calendar " + name + " is bound as a run-day (only) calendar by " + where +
				" and cannot also be global; a global calendar is unioned into every entry's SKIP set", nil
		}
	}
	return "", nil
}

// normalizeCalendarDays validates, de-duplicates and sorts a day list.
func normalizeCalendarDays(in []calendarDayInput) ([]calendarDayInput, string) {
	seen := map[string]bool{}
	out := make([]calendarDayInput, 0, len(in))
	for _, d := range in {
		day := strings.TrimSpace(d.Day)
		if !calendar.ValidDay(day) {
			return nil, "invalid day " + strconv.Quote(d.Day) + " (want a real YYYY-MM-DD date)"
		}
		if seen[day] {
			continue // a repeated date is the same fact twice, not an error
		}
		seen[day] = true
		out = append(out, calendarDayInput{Day: day, Label: strings.TrimSpace(d.Label)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, ""
}

func writeCalendarDays(ctx context.Context, tx *sql.Tx, name string, days []calendarDayInput) error {
	for _, d := range days {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_days(calendar_source, calendar_name, day, label)
			VALUES('amadeus', ?, ?, ?)`, name, d.Day, nullStrIf(d.Label)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) writeCalendar(w http.ResponseWriter, r *http.Request, name string, status int) {
	rows, err := s.loadCalendarRows(r.Context(), name)
	if err != nil || len(rows) == 0 {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, status, rows[0])
}

func (s *Server) auditCalendar(r *http.Request, actor, action, target, summary string) {
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Calendars", action, target, summary)
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Calendars", Action: action, Target: target, Summary: summary,
	})
}
