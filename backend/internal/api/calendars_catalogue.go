package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// Central's calendar catalogue and the agencies' copies (2.5.3 CA4a, CA-25 to
// CA-29).
//
// The catalogue is the built-in agency's calendars, written by global
// administrators through the ordinary calendar routes; no run reads them (a
// binding resolves among its own agency's calendars, calendar.Load). An agency
// takes a copy (Add from Central): its own row, with the catalogue calendar's
// content, a link to it (origin_uid) and the content it holds (origin_hash).
// A copy is read-only in its agency; it changes by an update from Central,
// asked for (Ask) or applied in the catalogue's own transaction (Update
// automatically), and becomes the agency's own when it is detached (Make it
// our own).
//
//	GET  /api/v1/calendars/catalogue               what Central offers (any calendar editor)
//	POST /api/v1/calendars/from-central            add a copy to the agency, or to agencies (a global administrator)
//	POST /api/v1/calendars/{name}/update           take the catalogue's latest
//	PUT  /api/v1/calendars/{name}/update-mode      ask | auto
//	POST /api/v1/calendars/{name}/detach           make it our own
//	POST /api/v1/calendars/catalogue/{uid}/push    a global administrator updates agencies' copies

// catalogueReservedName is the calendar name GET /calendars/catalogue takes
// from GET /calendars/{name}: a calendar of that name could not be read by
// name, so none is made with it.
const catalogueReservedName = "catalogue"

// The states of a copy.
const (
	copyUpToDate        = "up_to_date"
	copyUpdateAvailable = "update_available"
	copyNoLongerOffered = "no_longer_offered"
)

func (s *Server) mountCalendarCatalogue(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	compose := func(h http.HandlerFunc) http.Handler {
		return s.requirePerm(auth.PermCompose, func(p rolePermissions) bool { return p.Compose })(h)
	}
	owned := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			owner, _, ok := s.calendarRef(w, r)
			if !ok || !s.requireCalendarOwner(w, r, owner) {
				return
			}
			h(w, r)
		}
	}
	mux.Handle("GET /api/v1/calendars/catalogue", requireSession(http.HandlerFunc(s.listCalendarCatalogue)))
	mux.Handle("POST /api/v1/calendars/from-central", compose(s.addCalendarFromCentral))
	mux.Handle("POST /api/v1/calendars/{name}/update", compose(owned(s.updateCalendarFromCentral)))
	mux.Handle("PUT /api/v1/calendars/{name}/update-mode", compose(owned(s.setCalendarUpdateMode)))
	mux.Handle("POST /api/v1/calendars/{name}/detach", compose(owned(s.detachCalendar)))
	mux.Handle("POST /api/v1/calendars/catalogue/{uid}/push", compose(s.pushCatalogueCalendar))
}

// calendarOrigin is a copy's link to the catalogue, as the copy's agency sees
// it.
type calendarOrigin struct {
	CatalogueUID string `json:"catalogueUid"`
	// CatalogueName is the catalogue calendar's name now; empty once it is
	// gone.
	CatalogueName string `json:"catalogueName,omitempty"`
	UpdateMode    string `json:"updateMode"`
	State         string `json:"state"`
	// Pending is what an update would change (update_available).
	Pending *calendarPending `json:"pending,omitempty"`
	// Blocked says why the update cannot be applied while the agency's
	// bindings stand (CAL-27).
	Blocked string `json:"blocked,omitempty"`
}

type calendarPending struct {
	DaysAdded          []string `json:"daysAdded"`
	DaysRemoved        []string `json:"daysRemoved"`
	DaysRelabelled     []string `json:"daysRelabelled"`
	DescriptionChanged bool     `json:"descriptionChanged"`
	// Global and RecordSuppressed are the catalogue's values, given when the
	// update changes them.
	Global           *bool `json:"global,omitempty"`
	RecordSuppressed *bool `json:"recordSuppressed,omitempty"`
}

// calendarCopyRef is one agency's copy of a catalogue calendar.
type calendarCopyRef struct {
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	Name       string `json:"name"`
	UpdateMode string `json:"updateMode"`
	State      string `json:"state"`
	Blocked    string `json:"blocked,omitempty"`
}

// catalogueView is every calendar and every binding, read once, for the
// catalogue's questions.
type catalogueView struct {
	rows        []calendarRow
	byUID       map[string]*calendarRow
	bindings    map[string][]calendarBinding
	agencyNames map[string]string
}

func (s *Server) loadCatalogueView(ctx context.Context) (*catalogueView, error) {
	rows, err := s.loadCalendarRows(ctx, "", "")
	if err != nil {
		return nil, err
	}
	bindings, err := allCalendarBindings(ctx, s.db)
	if err != nil {
		return nil, err
	}
	v := &catalogueView{rows: rows, byUID: map[string]*calendarRow{}, bindings: bindings, agencyNames: map[string]string{}}
	for i := range v.rows {
		v.byUID[v.rows[i].UID] = &v.rows[i]
	}
	names, err := s.db.QueryContext(ctx, `SELECT id, name FROM agencies`)
	if err != nil {
		return nil, err
	}
	defer names.Close()
	for names.Next() {
		var id, name string
		if err := names.Scan(&id, &name); err != nil {
			return nil, err
		}
		v.agencyNames[id] = name
	}
	return v, names.Err()
}

func (v *catalogueView) find(owner, name string) *calendarRow {
	for i := range v.rows {
		if v.rows[i].OwnerAgency == owner && v.rows[i].Name == name {
			return &v.rows[i]
		}
	}
	return nil
}

// catalogueOf is the catalogue calendar a copy came from, or nil when it is
// gone (or the row is no copy).
func (v *catalogueView) catalogueOf(c *calendarRow) *calendarRow {
	if c.originUID == "" || c.originGone {
		return nil
	}
	if cat := v.byUID[c.originUID]; cat != nil && cat.OwnerAgency == agencyid.Global {
		return cat
	}
	return nil
}

func (v *catalogueView) copiesOf(uid string) []*calendarRow {
	var out []*calendarRow
	for i := range v.rows {
		if v.rows[i].originUID == uid {
			out = append(out, &v.rows[i])
		}
	}
	return out
}

func contentOf(c *calendarRow) calendar.Content {
	days := calendar.DaySet{}
	for _, d := range c.Days {
		days[d.Day] = d.Label
	}
	return calendar.Content{Description: c.Description, Global: c.Global, RecordSuppressed: c.RecordSuppressed, Days: days}
}

// linkOf is a copy's state against the catalogue calendar's content `to` (the
// catalogue's as it stands, or as a save is about to make it); nil for a row
// that is no copy.
func (v *catalogueView) linkOf(c *calendarRow, cat *calendarRow) *calendarOrigin {
	if c.originUID == "" {
		return nil
	}
	o := &calendarOrigin{CatalogueUID: c.originUID, UpdateMode: c.updateMode, State: copyUpToDate}
	if cat == nil {
		o.State = copyNoLongerOffered
		return o
	}
	o.CatalogueName = cat.Name
	to := contentOf(cat)
	d := calendar.Diff(contentOf(c), to)
	if calendar.ContentHash(to) == c.originHash || d.Empty() {
		return o
	}
	o.State = copyUpdateAvailable
	o.Pending = &calendarPending{
		DaysAdded: nonNil(d.DaysAdded), DaysRemoved: nonNil(d.DaysRemoved), DaysRelabelled: nonNil(d.DaysRelabelled),
		DescriptionChanged: d.DescriptionChanged,
	}
	if d.GlobalChanged {
		o.Pending.Global = &to.Global
	}
	if d.RecordSuppressedChanged {
		o.Pending.RecordSuppressed = &to.RecordSuppressed
	}
	o.Blocked = onlyBindingReason(v.bindings, c.OwnerAgency, c.Name, to.Global)
	return o
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// annotate gives a row its link (a copy) or its copies (a catalogue
// calendar).
func (v *catalogueView) annotate(c *calendarRow) {
	c.FromCentral = v.linkOf(c, v.catalogueOf(c))
	if c.OwnerAgency != agencyid.Global {
		return
	}
	c.Copies = nil
	for _, cp := range v.copiesOf(c.UID) {
		ref := calendarCopyRef{AgencyID: cp.OwnerAgency, AgencyName: v.agencyNames[cp.OwnerAgency], Name: cp.Name, UpdateMode: cp.updateMode, State: copyNoLongerOffered}
		if l := v.linkOf(cp, v.catalogueOf(cp)); l != nil {
			ref.State, ref.Blocked = l.State, l.Blocked
		}
		c.Copies = append(c.Copies, ref)
	}
}

// onlyBindingReason is CAL-27's refusal of a global (freeze) calendar that an
// entry keeps to (binds as only): what binds it, or "" when nothing does or
// the calendar is not to be global. Bindings of a binned owner count (FX2-C1).
func onlyBindingReason(bindings map[string][]calendarBinding, owner, name string, global bool) string {
	if !global {
		return ""
	}
	for _, b := range bindings[calKey(owner, name)] {
		if b.Polarity == "only" {
			where := bindingRef(b)
			if b.BinnedOwner {
				where += " (in recycle bin)"
			}
			return "calendar " + name + " is bound as a run-day (only) calendar by " + where +
				" and cannot also be a freeze calendar; a freeze calendar is unioned into every entry's SKIP set"
		}
	}
	return ""
}

// refuseCopyWrite answers 409 calendar_from_central for a write to a copy's
// days, description or flags (CA-27): it changes only by an update from
// Central until it is made the agency's own. Written on true.
func (s *Server) refuseCopyWrite(w http.ResponseWriter, r *http.Request, owner, name string) bool {
	var origin sql.NullString
	if err := s.db.QueryRowContext(r.Context(), `SELECT origin_uid FROM calendars WHERE owner_agency = ? AND name = ?`, owner, name).Scan(&origin); err != nil {
		if err == sql.ErrNoRows {
			return false
		}
		httpx.Fail500(w, s.log, "db_error", err)
		return true
	}
	if !origin.Valid || origin.String == "" {
		return false
	}
	httpx.Fail(w, http.StatusConflict, "calendar_from_central",
		"this calendar is a copy of one from Central: it changes only by an update from Central. Make it our own to edit it")
	return true
}

// applyFrom writes the content of `from` (a catalogue calendar) into the copy,
// and the hash of what it now holds. The copy was read before the
// transaction: it is written only while it is still a copy of `from` (not
// made its agency's own, deleted or replaced by a calendar of the name in the
// meantime, the review of CA4a); false when it no longer is, and nothing is
// written.
func applyFrom(ctx context.Context, tx *sql.Tx, copy *calendarRow, from *calendarRow, actor, now string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE calendars SET description = ?, global = ?, record_suppressed = ?, origin_hash = ?,
		                     last_modified_by = ?, last_modified_at = ?
		 WHERE owner_agency = ? AND name = ? AND uid = ? AND origin_uid = ? AND origin_gone = 0`,
		nullStrIf(from.Description), boolInt(from.Global), boolInt(from.RecordSuppressed), calendar.ContentHash(contentOf(from)),
		actor, now, copy.OwnerAgency, copy.Name, copy.UID, from.UID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_days WHERE calendar_owner = ? AND calendar_name = ?`, copy.OwnerAgency, copy.Name); err != nil {
		return false, err
	}
	return true, writeCalendarDays(ctx, tx, copy.OwnerAgency, copy.Name, from.Days)
}

// failCopyChanged answers a copy that changed while it was being updated.
func failCopyChanged(w http.ResponseWriter) {
	httpx.Fail(w, http.StatusConflict, "conflict", "the calendar changed while it was being updated: read it again")
}

// auditCalendarTx is auditCalendar inside a transaction, for a change made in
// an agency by a write elsewhere (an automatic update, a push).
func auditCalendarTx(ctx context.Context, tx *sql.Tx, agency, actor, action, target, summary string) error {
	if err := auditlog.WriteChangeLog(ctx, tx, agency, actor, "Calendars", action, target, summary); err != nil {
		return err
	}
	return auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
		AgencyID: agency, Kind: "config", Actor: actor, Category: "Calendars", Action: action, Target: target, Summary: summary,
	})
}

// catalogueSave is what a save of a catalogue calendar did to its copies.
type catalogueSave struct {
	// changed: the save changed the catalogue calendar's content. A save that
	// changes nothing writes no notice and raises no event.
	changed bool
	updated []*calendarRow // updated automatically, in the save's transaction
	behind  []*calendarRow // Ask copies now behind, and automatic ones the agency's bindings held back
	all     []string       // every copy's uid, whose notices the save settles
}

// propagateCatalogue updates the automatic copies of a catalogue calendar to
// `next`, the content the save gives it, in the save's transaction (CA-28):
// each that its agency's bindings allow (CAL-27), with a change-log row in
// its agency. The others, and every Ask copy the save puts behind, are listed
// for the notices and alert events written after the commit.
func propagateCatalogue(ctx context.Context, tx *sql.Tx, v *catalogueView, next *calendarRow, actor, now string) (catalogueSave, error) {
	var res catalogueSave
	if cur := v.byUID[next.UID]; cur != nil && calendar.ContentHash(contentOf(cur)) == calendar.ContentHash(contentOf(next)) {
		return res, nil
	}
	res.changed = true
	for _, cp := range v.copiesOf(next.UID) {
		res.all = append(res.all, cp.UID)
		if cp.originGone {
			continue
		}
		l := v.linkOf(cp, next)
		if l == nil || l.State != copyUpdateAvailable {
			continue
		}
		if cp.updateMode != "auto" || l.Blocked != "" {
			res.behind = append(res.behind, cp)
			continue
		}
		applied, err := applyFrom(ctx, tx, cp, next, actor, now)
		if err != nil {
			return res, err
		}
		if !applied {
			continue // no longer this calendar's copy
		}
		if err := auditCalendarTx(ctx, tx, cp.OwnerAgency, actor, "Updated", cp.Name, "Updated from Central's "+next.Name+" automatically"); err != nil {
			return res, err
		}
		res.updated = append(res.updated, cp)
	}
	return res, nil
}

// afterCatalogueSave writes the notices and raises the alert events of a
// catalogue save, after its commit.
func (s *Server) afterCatalogueSave(r *http.Request, catName string, res catalogueSave) {
	if !res.changed {
		return
	}
	for _, cp := range res.updated {
		s.raiseCalendarAlert(r, notify.TriggerCalendarUpdated, cp.OwnerAgency, cp.Name, catName,
			"was updated automatically from Central's "+catName)
	}
	for _, cp := range res.behind {
		s.raiseCalendarAlert(r, notify.TriggerCalendarUpdateAvailable, cp.OwnerAgency, cp.Name, catName,
			"is behind Central's "+catName+", which has changed: an update is available")
	}
	// Every copy, not only those the save touched: one the save brought level
	// again (a change undone) has its notice resolved (the review of CA4a).
	s.settleCopyNotices(r.Context(), res.all)
}

// settleCopyNotices brings the notices of the given copies (by uid) to their
// state now: a copy behind its catalogue calendar has calendar_update_available,
// one whose catalogue calendar is gone has calendar_no_longer_offered, and a
// copy up to date, made its agency's own, or deleted has neither.
func (s *Server) settleCopyNotices(ctx context.Context, uids []string) {
	if len(uids) == 0 {
		return
	}
	v, err := s.loadCatalogueView(ctx)
	if err != nil {
		s.log.Warn("calendar copies: notices not settled", "err", err)
		return
	}
	for _, uid := range uids {
		subject := "calendar:" + uid
		c := v.byUID[uid]
		var l *calendarOrigin
		if c != nil {
			l = v.linkOf(c, v.catalogueOf(c))
		}
		var err error
		switch {
		case l != nil && l.State == copyUpdateAvailable:
			err = notices.Upsert(ctx, s.db, notices.KindCalendarUpdateAvailable, notices.Finding{
				AgencyID: c.OwnerAgency, Subject: subject, Detail: updateAvailableDetail(c, l),
			})
			if err == nil {
				err = notices.Resolve(ctx, s.db, notices.KindCalendarNoLongerOffered, subject)
			}
		case l != nil && l.State == copyNoLongerOffered:
			err = notices.Upsert(ctx, s.db, notices.KindCalendarNoLongerOffered, notices.Finding{
				AgencyID: c.OwnerAgency, Subject: subject,
				Detail: "Central no longer offers the calendar " + c.Name + " was copied from. It stays as it is, and is " +
					"read-only: make it our own to edit it (Schedules → Calendars), or delete it.",
			})
			if err == nil {
				err = notices.Resolve(ctx, s.db, notices.KindCalendarUpdateAvailable, subject)
			}
		default:
			err = notices.Resolve(ctx, s.db, notices.KindCalendarUpdateAvailable, subject)
			if err == nil {
				err = notices.Resolve(ctx, s.db, notices.KindCalendarNoLongerOffered, subject)
			}
		}
		if err != nil {
			s.log.Warn("calendar copies: notice not settled", "calendar", uid, "err", err)
		}
	}
}

func updateAvailableDetail(c *calendarRow, l *calendarOrigin) string {
	var changes []string
	if n := len(l.Pending.DaysAdded); n > 0 {
		changes = append(changes, strconv.Itoa(n)+" day(s) added")
	}
	if n := len(l.Pending.DaysRemoved); n > 0 {
		changes = append(changes, strconv.Itoa(n)+" removed")
	}
	if n := len(l.Pending.DaysRelabelled); n > 0 {
		changes = append(changes, strconv.Itoa(n)+" relabelled")
	}
	if l.Pending.DescriptionChanged {
		changes = append(changes, "its description")
	}
	if g := l.Pending.Global; g != nil {
		if *g {
			changes = append(changes, "it becomes a freeze calendar")
		} else {
			changes = append(changes, "it is no longer a freeze calendar")
		}
	}
	if rs := l.Pending.RecordSuppressed; rs != nil {
		if *rs {
			changes = append(changes, "suppressed runs are recorded")
		} else {
			changes = append(changes, "suppressed runs are no longer recorded")
		}
	}
	d := fmt.Sprintf("Central has changed %s, which the calendar %s is a copy of (%s). ", l.CatalogueName, c.Name, strings.Join(changes, ", "))
	switch {
	case l.Blocked != "" && l.UpdateMode == "auto":
		d += "It updates automatically, but this update was not applied: " + l.Blocked + "."
	case l.Blocked != "":
		d += "The update cannot be applied while the agency's bindings stand: " + l.Blocked + "."
	default:
		d += "Update it from Schedules → Calendars, or make it our own to keep it as it is."
	}
	return d
}

// raiseCalendarAlert tells a copy's agency's alert rules (and the built-in
// agency's, which hear every agency's) of an update.
func (s *Server) raiseCalendarAlert(r *http.Request, trigger, agency, name, catName, what string) {
	agencyName := s.agencyNameOr(r, agency)
	notify.New(s.db, s.cfg, s.log).AlertRaised(notify.AlertEvent{
		Trigger: trigger, OwnerKind: "calendar", Agency: agency,
		Subject: fmt.Sprintf("[Cronomicon] Calendar %s of %s %s", name, agencyName, what),
		Detail: fmt.Sprintf("The calendar %s of %s %s.\nIts state is on Schedules → Calendars, under From Central.",
			name, agencyName, what),
	})
}

// composesSomewhere reports whether the caller writes calendars in some
// agency: a calendar editor, who may read the catalogue.
func composesSomewhere(id auth.Identity) bool {
	if id.GlobalAdmin(auth.PermCompose) {
		return true
	}
	for _, g := range id.RoleGrants() {
		if auth.PermsForRoles([]string{g.Role}).Has(auth.PermCompose) {
			return true
		}
	}
	return false
}

// catalogueItem is one calendar Central offers.
type catalogueItem struct {
	UID              string  `json:"uid"`
	Name             string  `json:"name"`
	Description      string  `json:"description,omitempty"`
	Global           bool    `json:"global"`
	RecordSuppressed bool    `json:"recordSuppressed"`
	DayCount         int     `json:"dayCount"`
	LastDay          *string `json:"lastDay"`
	Hash             string  `json:"hash"`
	// CopiedAs are the names of the copies the agency the request works in
	// holds of it.
	CopiedAs []string `json:"copiedAs,omitempty"`
}

func (s *Server) listCalendarCatalogue(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	if !composesSomewhere(id) {
		s.denyEntityAgency(w, r, id, auth.PermCompose, auth.AllScopes,
			"Central's calendar catalogue is for the agencies' calendar editors")
		return
	}
	v, err := s.loadCatalogueView(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	ctxAgency, named := agencyctx.From(r)
	items := []catalogueItem{}
	for i := range v.rows {
		c := &v.rows[i]
		if c.OwnerAgency != agencyid.Global {
			continue
		}
		it := catalogueItem{UID: c.UID, Name: c.Name, Description: c.Description, Global: c.Global, RecordSuppressed: c.RecordSuppressed,
			DayCount: c.DayCount, LastDay: c.LastDay, Hash: calendar.ContentHash(contentOf(c))}
		if named && agencyctx.Readable(ctxAgency) {
			for _, cp := range v.copiesOf(c.UID) {
				if cp.OwnerAgency == ctxAgency {
					it.CopiedAs = append(it.CopiedAs, cp.Name)
				}
			}
		}
		items = append(items, it)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

type fromCentralInput struct {
	CatalogueUID string `json:"catalogueUid"`
	// Name is the copy's name; the catalogue calendar's when empty.
	Name string `json:"name"`
	// UpdateMode is ask (the default) or auto.
	UpdateMode string `json:"updateMode"`
	// AgencyIDs are the agencies a global administrator adds it to; empty, the
	// agency the request works in.
	AgencyIDs []string `json:"agencyIds"`
}

func validUpdateMode(m string) bool { return m == "ask" || m == "auto" }

// addCalendarFromCentral is Add from Central (CA-26): a copy of a catalogue
// calendar in the agency the request works in, or, for a global
// administrator, in each agency named. Judged whole before anything is
// written.
func (s *Server) addCalendarFromCentral(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in fromCentralInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	if in.UpdateMode == "" {
		in.UpdateMode = "ask"
	}
	if !validUpdateMode(in.UpdateMode) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", `updateMode is "ask" or "auto"`)
		return
	}
	v, err := s.loadCatalogueView(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	cat := v.byUID[strings.TrimSpace(in.CatalogueUID)]
	if cat == nil || cat.OwnerAgency != agencyid.Global {
		httpx.Fail(w, http.StatusNotFound, "not_found", "Central's catalogue has no such calendar")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = cat.Name
	}
	if !composeNameRe.MatchString(name) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid calendar name (want "+composeNameRe.String()+")")
		return
	}
	if name == catalogueReservedName {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", `the name "catalogue" is reserved`)
		return
	}

	var targets []string
	acting := len(in.AgencyIDs) > 0
	if acting {
		if !id.GlobalAdmin(auth.PermCompose) {
			s.denyEntityAgency(w, r, id, auth.PermCompose, auth.AllScopes,
				"adding a calendar from Central to agencies is a global administrator's; an agency adds one working in it")
			return
		}
		for _, a := range in.AgencyIDs {
			if a = strings.TrimSpace(a); a != "" && !slices.Contains(targets, a) {
				targets = append(targets, a)
			}
		}
	} else {
		a, named := agencyctx.From(r)
		if !named || !agencyctx.Readable(a) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "agency_required",
				"say which agency the copy is for: work in it, or (a global administrator) name agencyIds")
			return
		}
		targets = []string{a}
	}
	for _, a := range targets {
		if a == agencyid.Global {
			httpx.Fail(w, http.StatusUnprocessableEntity, "catalogue_copies_nothing",
				"the catalogue is the built-in agency's own calendars: a copy is made in an agency")
			return
		}
		if _, known := v.agencyNames[a]; !known {
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "agencyIds names no agency: "+a)
			return
		}
		if !s.requireCalendarOwner(w, r, a) {
			return
		}
		if v.find(a, name) != nil {
			httpx.Fail(w, http.StatusConflict, "conflict", v.agencyNames[a]+" already has a calendar named "+name)
			return
		}
		// CAL-27, as a create checks it: a freeze calendar under a name an
		// entry keeps to (a binding left dangling by a forced delete).
		if reason := onlyBindingReason(v.bindings, a, name, cat.Global); reason != "" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", reason+" (in "+v.agencyNames[a]+")")
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
	hash := calendar.ContentHash(contentOf(cat))
	for _, a := range targets {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO calendars(source, name, description, global, record_suppressed,
			                      created_by, created_at, last_modified_by, last_modified_at, owner_agency,
			                      origin_uid, origin_hash, update_mode)
			VALUES('cronomicon', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			name, nullStrIf(cat.Description), boolInt(cat.Global), boolInt(cat.RecordSuppressed),
			id.Email, now, id.Email, now, a, cat.UID, hash, in.UpdateMode); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if err := writeCalendarDays(r.Context(), tx, a, name, cat.Days); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		ctx := r.Context()
		if acting {
			ctx = auditlog.WithActingAgency(ctx, a) // IN-9: acting in each
		}
		if err := auditCalendarTx(ctx, tx, a, id.Email, "Created", name,
			"Added from Central's "+cat.Name+" ("+strconv.Itoa(len(cat.Days))+" day(s), updates: "+in.UpdateMode+")"); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	out, err := s.loadCatalogueView(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	items := []calendarRow{}
	for _, a := range targets {
		if c := out.find(a, name); c != nil {
			out.annotate(c)
			items = append(items, *c)
		}
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"items": items})
}

// copyOrRefuse is the copy a {name} route means, with its link, or a refusal:
// 409 not_from_central for a calendar that is no copy. Written on nil.
func (s *Server) copyOrRefuse(w http.ResponseWriter, r *http.Request) (*catalogueView, *calendarRow, *calendarOrigin) {
	owner, name, ok := s.calendarRef(w, r)
	if !ok {
		return nil, nil, nil
	}
	v, err := s.loadCatalogueView(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, nil, nil
	}
	c := v.find(owner, name)
	if c == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "calendar not found")
		return nil, nil, nil
	}
	l := v.linkOf(c, v.catalogueOf(c))
	if l == nil {
		httpx.Fail(w, http.StatusConflict, "not_from_central", "this calendar is the agency's own, not a copy of one from Central")
		return nil, nil, nil
	}
	return v, c, l
}

func failNoLongerOffered(w http.ResponseWriter) {
	httpx.Fail(w, http.StatusConflict, "calendar_no_longer_offered",
		"Central no longer offers the calendar this one was copied from: it can be made our own, or deleted")
}

// updateCalendarFromCentral is Update (CA-28): the copy takes the catalogue
// calendar's content as it is now. Up to date already, it is answered as it
// is.
func (s *Server) updateCalendarFromCentral(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	v, c, l := s.copyOrRefuse(w, r)
	if c == nil {
		return
	}
	switch {
	case l.State == copyNoLongerOffered:
		failNoLongerOffered(w)
		return
	case l.State == copyUpToDate:
		s.settleCopyNotices(r.Context(), []string{c.UID}) // a notice left from before is resolved
		s.writeAnnotatedCalendar(w, r, c.OwnerAgency, c.Name, http.StatusOK)
		return
	case l.Blocked != "":
		s.settleCopyNotices(r.Context(), []string{c.UID}) // the notice says the reason as it is now
		httpx.Fail(w, http.StatusConflict, "calendar_update_blocked", "the update cannot be applied: "+l.Blocked)
		return
	}
	cat := v.catalogueOf(c)
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	if applied, err := applyFrom(r.Context(), tx, c, cat, id.Email, time.Now().UTC().Format(time.RFC3339)); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	} else if !applied {
		failCopyChanged(w)
		return
	}
	if err := auditCalendarTx(r.Context(), tx, c.OwnerAgency, id.Email, "Updated", c.Name, "Updated from Central's "+cat.Name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.settleCopyNotices(r.Context(), []string{c.UID})
	s.writeAnnotatedCalendar(w, r, c.OwnerAgency, c.Name, http.StatusOK)
}

// setCalendarUpdateMode changes how a copy takes the catalogue's changes. Set
// to auto while behind, it takes the pending update at once, when the
// agency's bindings allow it, as the next catalogue save would.
func (s *Server) setCalendarUpdateMode(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var body struct {
		UpdateMode string `json:"updateMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validUpdateMode(body.UpdateMode) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", `updateMode is "ask" or "auto"`)
		return
	}
	v, c, l := s.copyOrRefuse(w, r)
	if c == nil {
		return
	}
	if l.State == copyNoLongerOffered {
		failNoLongerOffered(w)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if res, err := tx.ExecContext(r.Context(), `UPDATE calendars SET update_mode = ?, last_modified_by = ?, last_modified_at = ?
	                                            WHERE owner_agency = ? AND name = ? AND uid = ? AND origin_uid IS NOT NULL`,
		body.UpdateMode, id.Email, now, c.OwnerAgency, c.Name, c.UID); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	} else if n, _ := res.RowsAffected(); n == 0 {
		failCopyChanged(w)
		return
	}
	summary := "Updates from Central: " + map[string]string{"ask": "ask", "auto": "automatic"}[body.UpdateMode]
	if body.UpdateMode == "auto" && l.State == copyUpdateAvailable && l.Blocked == "" {
		cat := v.catalogueOf(c)
		if applied, err := applyFrom(r.Context(), tx, c, cat, id.Email, now); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		} else if !applied {
			failCopyChanged(w)
			return
		}
		summary += "; updated from Central's " + cat.Name
	}
	if err := auditCalendarTx(r.Context(), tx, c.OwnerAgency, id.Email, "Updated", c.Name, summary); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.settleCopyNotices(r.Context(), []string{c.UID})
	s.writeAnnotatedCalendar(w, r, c.OwnerAgency, c.Name, http.StatusOK)
}

// detachCalendar is Make it our own (CA-27): the copy becomes an ordinary
// calendar of its agency, editable, with no link and no updates.
func (s *Server) detachCalendar(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	_, c, l := s.copyOrRefuse(w, r)
	if c == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE calendars SET origin_uid = NULL, origin_hash = NULL, update_mode = NULL, origin_gone = 0,
		                     last_modified_by = ?, last_modified_at = ?
		 WHERE owner_agency = ? AND name = ?`, id.Email, now, c.OwnerAgency, c.Name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	from := "a calendar Central no longer offers"
	if l.CatalogueName != "" {
		from = "Central's " + l.CatalogueName
	}
	s.auditCalendar(r, c.OwnerAgency, id.Email, "Updated", c.Name, "Made our own (it was a copy of "+from+")")
	s.settleCopyNotices(r.Context(), []string{c.UID})
	s.writeAnnotatedCalendar(w, r, c.OwnerAgency, c.Name, http.StatusOK)
}

// pushCatalogueCalendar is Push the update (CA-28): a global administrator
// updates the named agencies' copies of a catalogue calendar, acting in each.
// A copy up to date is left as it is, one the agency's bindings hold back is
// reported with the reason, and an agency with no copy is reported.
func (s *Server) pushCatalogueCalendar(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	if !id.GlobalAdmin(auth.PermCompose) {
		s.denyEntityAgency(w, r, id, auth.PermCompose, auth.AllScopes, "pushing an update from Central is a global administrator's")
		return
	}
	var body struct {
		AgencyIDs []string `json:"agencyIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.AgencyIDs) == 0 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "name the agencies to push the update to (agencyIds)")
		return
	}
	v, err := s.loadCatalogueView(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	cat := v.byUID[r.PathValue("uid")]
	if cat == nil || cat.OwnerAgency != agencyid.Global {
		httpx.Fail(w, http.StatusNotFound, "not_found", "Central's catalogue has no such calendar")
		return
	}
	type pushed struct {
		AgencyID string `json:"agencyId"`
		Name     string `json:"name"`
		Reason   string `json:"reason,omitempty"`
	}
	out := struct {
		Updated  []pushed `json:"updated"`
		UpToDate []pushed `json:"upToDate"`
		Blocked  []pushed `json:"blocked"`
		NoCopy   []string `json:"noCopy"`
	}{Updated: []pushed{}, UpToDate: []pushed{}, Blocked: []pushed{}, NoCopy: []string{}}
	var apply []*calendarRow
	var seen []string
	for _, a := range body.AgencyIDs {
		if a = strings.TrimSpace(a); a == "" || slices.Contains(seen, a) {
			continue
		}
		seen = append(seen, a)
		found := false
		for _, cp := range v.copiesOf(cat.UID) {
			if cp.OwnerAgency != a || cp.originGone {
				continue
			}
			found = true
			l := v.linkOf(cp, cat)
			switch {
			case l.State == copyUpToDate:
				out.UpToDate = append(out.UpToDate, pushed{AgencyID: a, Name: cp.Name})
			case l.Blocked != "":
				out.Blocked = append(out.Blocked, pushed{AgencyID: a, Name: cp.Name, Reason: l.Blocked})
			default:
				apply = append(apply, cp)
				out.Updated = append(out.Updated, pushed{AgencyID: a, Name: cp.Name})
			}
		}
		if !found {
			out.NoCopy = append(out.NoCopy, a)
		}
	}
	if len(apply) > 0 {
		tx, err := s.db.BeginTx(r.Context(), nil)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		defer tx.Rollback()
		now := time.Now().UTC().Format(time.RFC3339)
		var done []*calendarRow
		for _, cp := range apply {
			applied, err := applyFrom(r.Context(), tx, cp, cat, id.Email, now)
			if err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if !applied {
				continue // no longer a copy of it: not pushed
			}
			done = append(done, cp)
			ctx := auditlog.WithActingAgency(r.Context(), cp.OwnerAgency) // IN-9
			if err := auditCalendarTx(ctx, tx, cp.OwnerAgency, id.Email, "Updated", cp.Name, "Updated from Central's "+cat.Name+" (pushed)"); err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		out.Updated = out.Updated[:0]
		for _, cp := range done {
			out.Updated = append(out.Updated, pushed{AgencyID: cp.OwnerAgency, Name: cp.Name})
		}
		apply = done
	}
	var uids []string
	for _, cp := range apply {
		uids = append(uids, cp.UID)
		s.raiseCalendarAlert(r, notify.TriggerCalendarUpdated, cp.OwnerAgency, cp.Name, cat.Name,
			"was updated from Central's "+cat.Name+" by a global administrator")
	}
	s.settleCopyNotices(r.Context(), uids)
	httpx.JSON(w, http.StatusOK, out)
}

// writeAnnotatedCalendar answers one calendar with its link or copies.
func (s *Server) writeAnnotatedCalendar(w http.ResponseWriter, r *http.Request, owner, name string, status int) {
	v, err := s.loadCatalogueView(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	c := v.find(owner, name)
	if c == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "calendar not found")
		return
	}
	v.annotate(c)
	httpx.JSON(w, status, *c)
}

// bindingRef names a binding in a refusal: kind:owner/entry, or kind:owner
// (its own Allow or Forbid) for a definition's own calendar.
func bindingRef(b calendarBinding) string {
	if b.Level == "definition" {
		return b.OwnerKind + ":" + b.OwnerName + " (its own " + map[string]string{"skip": "Forbid", "only": "Allow"}[b.Polarity] + ")"
	}
	return b.OwnerKind + ":" + b.OwnerName + "/" + b.ScheduleName
}
