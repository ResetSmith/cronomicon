package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// 2.5.3 CA4a (CA-25 to CA-29): Central's calendars are a catalogue; an agency
// adds a linked, read-only copy, which takes the catalogue's changes when the
// agency updates it (Ask) or in the catalogue's own transaction (Update
// automatically), unless the agency's bindings forbid the change (CAL-27).

type calOrigin struct {
	CatalogueUID  string `json:"catalogueUid"`
	CatalogueName string `json:"catalogueName"`
	UpdateMode    string `json:"updateMode"`
	State         string `json:"state"`
	Blocked       string `json:"blocked"`
	Pending       *struct {
		DaysAdded   []string `json:"daysAdded"`
		DaysRemoved []string `json:"daysRemoved"`
		Global      *bool    `json:"global"`
	} `json:"pending"`
}

type calRow struct {
	Name        string `json:"name"`
	UID         string `json:"uid"`
	OwnerAgency string `json:"ownerAgency"`
	Global      bool   `json:"global"`
	CanEdit     bool   `json:"canEdit"`
	CanDelete   bool   `json:"canDelete"`
	Days        []struct {
		Day string `json:"day"`
	} `json:"days"`
	FromCentral *calOrigin `json:"fromCentral"`
	Copies      []struct {
		AgencyID string `json:"agencyId"`
		Name     string `json:"name"`
		State    string `json:"state"`
	} `json:"copies"`
}

type catalogueFixture struct {
	t    *testing.T
	h    http.Handler
	pool *sql.DB
}

func newCatalogueFixture(t *testing.T) *catalogueFixture {
	h, pool := gateServer(t)
	return &catalogueFixture{t: t, h: h, pool: pool}
}

// in is a request working in an agency (the switcher's header), or in none.
func (f *catalogueFixture) in(agency, who, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	if agency == "" {
		return gateReq(f.t, f.h, method, path, who, body)
	}
	return gateReqWithHeader(f.t, f.h, method, path, who, body, auth.AgencyHeader, agency)
}

// catalogue makes a calendar of Central's, a global administrator's.
func (f *catalogueFixture) catalogue(name string, days ...string) string {
	f.t.Helper()
	var ds []string
	for _, d := range days {
		ds = append(ds, `{"day":"`+d+`","label":"L `+d+`"}`)
	}
	rec := gateReq(f.t, f.h, http.MethodPost, "/api/v1/calendars", gRoot,
		`{"name":"`+name+`","ownerAgency":"global","days":[`+strings.Join(ds, ",")+`]}`)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("a global administrator making %s in the catalogue = %d (%s)", name, rec.Code, rec.Body)
	}
	return f.get("global", name).UID
}

func (f *catalogueFixture) get(agency, name string) calRow {
	f.t.Helper()
	rec := gateReq(f.t, f.h, http.MethodGet, "/api/v1/calendars/"+name+"?agencyId="+agency, gRoot, "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET %s of %s = %d (%s)", name, agency, rec.Code, rec.Body)
	}
	var c calRow
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *catalogueFixture) setDays(agency, name string, days ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	var ds []string
	// The labels the catalogue fixture gives, so a change is the days alone.
	for _, d := range days {
		ds = append(ds, `{"day":"`+d+`","label":"L `+d+`"}`)
	}
	return gateReq(f.t, f.h, http.MethodPut, "/api/v1/calendars/"+name+"/days?agencyId="+agency, gRoot, `{"days":[`+strings.Join(ds, ",")+`]}`)
}

func (f *catalogueFixture) openNotice(kind, subject string) (agency, detail string, open bool) {
	f.t.Helper()
	err := f.pool.QueryRow(`SELECT agency_id, detail FROM notices WHERE kind = ? AND subject = ? AND resolved_at IS NULL`, kind, subject).Scan(&agency, &detail)
	return agency, detail, err == nil
}

func dayList(c calRow) string {
	var out []string
	for _, d := range c.Days {
		out = append(out, d.Day)
	}
	return strings.Join(out, ",")
}

func TestCA4a_AnAgencyAddsACalendarFromCentral(t *testing.T) {
	f := newCatalogueFixture(t)
	uid := f.catalogue("holidays", "2026-12-25", "2027-01-01")

	// The catalogue is read by any agency's calendar editor, not by a viewer.
	rec := f.in("ag:FIN", gFinAdmin, http.MethodGet, "/api/v1/calendars/catalogue", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), uid) {
		t.Fatalf("FIN's administrator reading the catalogue = %d (%s)", rec.Code, rec.Body)
	}
	if code := f.in("ag:FIN", gFinViewer, http.MethodGet, "/api/v1/calendars/catalogue", "").Code; code != http.StatusForbidden {
		t.Errorf("a FIN viewer reading the catalogue = %d, want 403", code)
	}

	add := func(agency, who, body string) *httptest.ResponseRecorder {
		return f.in(agency, who, http.MethodPost, "/api/v1/calendars/from-central", body)
	}
	if rec := add("ag:FIN", gFinAdmin, `{"catalogueUid":"`+uid+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("FIN adding holidays from Central = %d (%s)", rec.Code, rec.Body)
	}
	cp := f.get("ag:FIN", "holidays")
	if cp.FromCentral == nil || cp.FromCentral.CatalogueUID != uid || cp.FromCentral.State != "up_to_date" || cp.FromCentral.UpdateMode != "ask" ||
		dayList(cp) != "2026-12-25,2027-01-01" || cp.UID == uid {
		t.Errorf("FIN's copy = %+v (days %s), want the catalogue's days, linked, up to date, ask", cp, dayList(cp))
	}
	if code := add("ag:FIN", gFinAdmin, `{"catalogueUid":"`+uid+`"}`).Code; code != http.StatusConflict {
		t.Errorf("adding it again under a name FIN holds = %d, want 409", code)
	}
	if rec := add("ag:TAX", gTaxAdmin, `{"catalogueUid":"`+uid+`","name":"tax-holidays","updateMode":"auto"}`); rec.Code != http.StatusCreated {
		t.Fatalf("TAX adding it under its own name = %d (%s)", rec.Code, rec.Body)
	}
	if c := f.get("ag:TAX", "tax-holidays"); c.FromCentral == nil || c.FromCentral.UpdateMode != "auto" {
		t.Errorf("TAX's copy = %+v, want linked, automatic", c)
	}
	if rec := f.in("ag:FIN", gFinAdmin, http.MethodGet, "/api/v1/calendars/catalogue", ""); !strings.Contains(rec.Body.String(), `"copiedAs":["holidays"]`) {
		t.Errorf("the catalogue does not say FIN holds a copy: %s", rec.Body)
	}
	if cat := f.get("global", "holidays"); len(cat.Copies) != 2 {
		t.Errorf("the catalogue calendar lists %+v, want FIN's and TAX's copies", cat.Copies)
	}

	// Who and where.
	if rec := add("", gFinAdmin, `{"catalogueUid":"`+uid+`","name":"x"}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "agency_required" {
		t.Errorf("adding with no agency = %d %s, want 422 agency_required", rec.Code, rec.Body)
	}
	if code := add("ag:FIN", gFinAdmin, `{"catalogueUid":"`+uid+`","name":"x","agencyIds":["ag:TAX"]}`).Code; code != http.StatusForbidden {
		t.Errorf("FIN's administrator naming agencyIds = %d, want 403 (a global administrator's)", code)
	}
	// Working in an agency it does not read is working in none.
	if rec := add("ag:TAX", gFinAdmin, `{"catalogueUid":"`+uid+`","name":"x"}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "agency_required" {
		t.Errorf("FIN's administrator adding to TAX = %d %s, want 422 agency_required", rec.Code, rec.Body)
	}
	if rec := add("", gRoot, `{"catalogueUid":"`+uid+`","name":"x","agencyIds":["global"]}`); errCode(rec.Body.Bytes()) != "catalogue_copies_nothing" {
		t.Errorf("a copy into the built-in agency = %d %s, want catalogue_copies_nothing", rec.Code, rec.Body)
	}

	// A global administrator adds one to two agencies at once, acting in each.
	close := f.catalogue("close", "2026-12-31")
	if rec := add("", gRoot, `{"catalogueUid":"`+close+`","agencyIds":["ag:FIN","ag:TAX"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("a global administrator adding close to FIN and TAX = %d (%s)", rec.Code, rec.Body)
	}
	for _, a := range []string{"ag:FIN", "ag:TAX"} {
		if n := count(t, f.pool, `SELECT COUNT(*) FROM change_log WHERE category = 'Calendars' AND target = 'close' AND agency_id = ? AND acting_agency = ?`, a, a); n != 1 {
			t.Errorf("%d change-log rows in %s acting in it for the copy, want 1", n, a)
		}
	}
	// One refusal is the whole answer: FIN holds close now.
	another := f.catalogue("other", "2026-11-11")
	if code := add("", gRoot, `{"catalogueUid":"`+another+`","name":"close","agencyIds":["ag:TAX","ag:FIN"]}`).Code; code != http.StatusConflict {
		t.Errorf("adding under a name both hold = %d, want 409", code)
	}
	if n := count(t, f.pool, `SELECT COUNT(*) FROM calendars WHERE origin_uid = ?`, another); n != 0 {
		t.Errorf("a refused add made %d copies", n)
	}

	// The route's name is no calendar's.
	if rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars", gRoot, `{"name":"catalogue","ownerAgency":"global"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a calendar named catalogue = %d, want 422", rec.Code)
	}
}

func TestCA4a_ACopyIsReadOnlyUntilMadeOurOwn(t *testing.T) {
	f := newCatalogueFixture(t)
	uid := f.catalogue("holidays", "2026-12-25")
	if rec := f.in("ag:FIN", gFinAdmin, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d (%s)", rec.Code, rec.Body)
	}
	put := func(path, body string) *httptest.ResponseRecorder {
		return gateReq(t, f.h, http.MethodPut, path, gFinAdmin, body)
	}
	if rec := put("/api/v1/calendars/holidays?agencyId=ag:FIN", `{"description":"ours"}`); rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "calendar_from_central" {
		t.Errorf("editing a copy = %d %s, want 409 calendar_from_central", rec.Code, rec.Body)
	}
	if rec := put("/api/v1/calendars/holidays/days?agencyId=ag:FIN", `{"days":[]}`); errCode(rec.Body.Bytes()) != "calendar_from_central" {
		t.Errorf("replacing a copy's days = %d %s, want 409 calendar_from_central", rec.Code, rec.Body)
	}
	rec := f.in("ag:FIN", gFinAdmin, http.MethodGet, "/api/v1/calendars", "")
	var list struct{ Items []calRow }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, c := range list.Items {
		if c.Name == "holidays" && (c.CanEdit || !c.CanDelete) {
			t.Errorf("the list has the copy canEdit %v canDelete %v, want read-only and deletable", c.CanEdit, c.CanDelete)
		}
	}

	if rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/holidays/detach?agencyId=ag:FIN", gFinAdmin, ""); rec.Code != http.StatusOK {
		t.Fatalf("make it our own = %d (%s)", rec.Code, rec.Body)
	}
	if c := f.get("ag:FIN", "holidays"); c.FromCentral != nil {
		t.Errorf("the calendar made its own is still linked: %+v", c.FromCentral)
	}
	if rec := put("/api/v1/calendars/holidays/days?agencyId=ag:FIN", `{"days":[{"day":"2026-07-04"}]}`); rec.Code != http.StatusOK {
		t.Errorf("editing the calendar made its own = %d (%s)", rec.Code, rec.Body)
	}
	// Unlinked: a catalogue change no longer reaches it.
	f.setDays("global", "holidays", "2026-12-25", "2026-12-26")
	if c := f.get("ag:FIN", "holidays"); dayList(c) != "2026-07-04" || c.FromCentral != nil {
		t.Errorf("the calendar made its own followed the catalogue: %s %+v", dayList(c), c.FromCentral)
	}
	if code := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/holidays/update?agencyId=ag:FIN", gFinAdmin, "").Code; code != http.StatusConflict {
		t.Errorf("updating a calendar that is no copy = %d, want 409", code)
	}
}

func TestCA4a_AskWaitsForTheAgencysUpdate(t *testing.T) {
	f := newCatalogueFixture(t)
	exec := mustExec(t, f.pool)
	exec(`INSERT INTO notification_config (id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
	      VALUES (1, '127.0.0.1', 1, 'cronomicon@example.com', 'starttls', '[]', 0, '', '[]', 'now')
	      ON CONFLICT(id) DO UPDATE SET smtp_host = excluded.smtp_host, smtp_port = excluded.smtp_port,
	        smtp_from = excluded.smtp_from, smtp_encryption = excluded.smtp_encryption, last_email_at = NULL`)
	exec(`INSERT INTO alert_config (id, target_mode, job_name, trigger, channels, recipients, enabled, created_at, owner_agency)
	      VALUES ('fin-cal', 'all', NULL, 'calendar-update-available', '["email"]', 'fin@example.com', 1, 'now', 'ag:FIN')`)
	uid := f.catalogue("holidays", "2026-12-25", "2027-01-01")
	if rec := f.in("ag:FIN", gFinAdmin, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d (%s)", rec.Code, rec.Body)
	}
	copyUID := f.get("ag:FIN", "holidays").UID

	if rec := f.setDays("global", "holidays", "2026-12-25", "2026-12-26"); rec.Code != http.StatusOK {
		t.Fatalf("a catalogue change = %d (%s)", rec.Code, rec.Body)
	}
	c := f.get("ag:FIN", "holidays")
	if c.FromCentral == nil || c.FromCentral.State != "update_available" || c.FromCentral.Pending == nil ||
		strings.Join(c.FromCentral.Pending.DaysAdded, ",") != "2026-12-26" || strings.Join(c.FromCentral.Pending.DaysRemoved, ",") != "2027-01-01" {
		t.Fatalf("FIN's copy after the change = %+v", c.FromCentral)
	}
	if dayList(c) != "2026-12-25,2027-01-01" {
		t.Errorf("an Ask copy changed by itself: %s", dayList(c))
	}
	if agency, detail, open := f.openNotice("calendar_update_available", "calendar:"+copyUID); !open || agency != "ag:FIN" || !strings.Contains(detail, "1 day(s) added") {
		t.Errorf("the notice = %q %q open %v, want FIN's, counting the change", agency, detail, open)
	}
	sent := func() string {
		var s string
		_ = f.pool.QueryRow(`SELECT COALESCE(last_email_at, '') FROM notification_config WHERE id = 1`).Scan(&s)
		return s
	}
	for deadline := time.Now().Add(5 * time.Second); sent() == "" && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if sent() == "" {
		t.Error("FIN's calendar-update-available rule heard nothing")
	}

	// Two changes while behind are one update, to the latest.
	f.setDays("global", "holidays", "2026-12-25", "2026-12-26", "2026-12-31")
	if n := count(t, f.pool, `SELECT COUNT(*) FROM notices WHERE kind = 'calendar_update_available' AND resolved_at IS NULL`); n != 1 {
		t.Errorf("%d open notices after two changes, want 1", n)
	}
	if code := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/holidays/update?agencyId=ag:FIN", gFinViewer, "").Code; code != http.StatusForbidden {
		t.Errorf("a FIN viewer updating = %d, want 403", code)
	}
	if rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/holidays/update?agencyId=ag:FIN", gFinAdmin, ""); rec.Code != http.StatusOK {
		t.Fatalf("FIN updating = %d (%s)", rec.Code, rec.Body)
	}
	if c := f.get("ag:FIN", "holidays"); c.FromCentral.State != "up_to_date" || dayList(c) != "2026-12-25,2026-12-26,2026-12-31" {
		t.Errorf("after Update: %s %+v, want the catalogue's latest, up to date", dayList(c), c.FromCentral)
	}
	if _, _, open := f.openNotice("calendar_update_available", "calendar:"+copyUID); open {
		t.Error("the notice is still open after the update")
	}

	// A global administrator pushes the next change to FIN and TAX (which has no
	// copy), acting in FIN.
	f.setDays("global", "holidays", "2026-12-25")
	if code := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/catalogue/"+uid+"/push", gFinAdmin, `{"agencyIds":["ag:FIN"]}`).Code; code != http.StatusForbidden {
		t.Errorf("FIN's administrator pushing = %d, want 403", code)
	}
	rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/catalogue/"+uid+"/push", gRoot, `{"agencyIds":["ag:FIN","ag:TAX"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"noCopy":["ag:TAX"]`) || !strings.Contains(rec.Body.String(), `"updated":[{"agencyId":"ag:FIN","name":"holidays"}]`) {
		t.Fatalf("push = %d (%s)", rec.Code, rec.Body)
	}
	if c := f.get("ag:FIN", "holidays"); dayList(c) != "2026-12-25" || c.FromCentral.State != "up_to_date" {
		t.Errorf("after the push: %s %+v", dayList(c), c.FromCentral)
	}
	if n := count(t, f.pool, `SELECT COUNT(*) FROM change_log WHERE agency_id = 'ag:FIN' AND acting_agency = 'ag:FIN' AND details LIKE '%(pushed)%'`); n != 1 {
		t.Errorf("%d change-log rows of the push in FIN acting in it, want 1", n)
	}
	if _, _, open := f.openNotice("calendar_update_available", "calendar:"+copyUID); open {
		t.Error("the notice is open after the push")
	}
}

func TestCA4a_AutoUpdatesInTheCataloguesSave(t *testing.T) {
	f := newCatalogueFixture(t)
	uid := f.catalogue("holidays", "2026-12-25")
	if rec := f.in("ag:FIN", gFinAdmin, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`","updateMode":"auto"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d (%s)", rec.Code, rec.Body)
	}
	if rec := f.setDays("global", "holidays", "2026-12-25", "2026-12-26"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if c := f.get("ag:FIN", "holidays"); dayList(c) != "2026-12-25,2026-12-26" || c.FromCentral.State != "up_to_date" {
		t.Errorf("the automatic copy after the save: %s %+v", dayList(c), c.FromCentral)
	}
	if rec := gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/holidays?agencyId=global", gRoot, `{"description":"Public holidays"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if n := count(t, f.pool, `SELECT COUNT(*) FROM calendars WHERE owner_agency = 'ag:FIN' AND name = 'holidays' AND description = 'Public holidays'`); n != 1 {
		t.Error("the description did not reach the automatic copy")
	}
	if n := count(t, f.pool, `SELECT COUNT(*) FROM change_log WHERE agency_id = 'ag:FIN' AND target = 'holidays' AND details LIKE '%automatically%'`); n != 2 {
		t.Errorf("%d change-log rows in FIN for two automatic updates, want 2", n)
	}
	if n := count(t, f.pool, `SELECT COUNT(*) FROM notices WHERE kind = 'calendar_update_available' AND resolved_at IS NULL`); n != 0 {
		t.Errorf("an automatic update left %d notices", n)
	}

	// Switched to Ask, then back to Update automatically while behind: it
	// takes the pending update at once.
	gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/holidays/update-mode?agencyId=ag:FIN", gFinAdmin, `{"updateMode":"ask"}`)
	f.setDays("global", "holidays", "2026-12-31")
	if c := f.get("ag:FIN", "holidays"); c.FromCentral.State != "update_available" {
		t.Fatalf("an Ask copy after the change: %+v", c.FromCentral)
	}
	if rec := gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/holidays/update-mode?agencyId=ag:FIN", gFinAdmin, `{"updateMode":"auto"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if c := f.get("ag:FIN", "holidays"); dayList(c) != "2026-12-31" || c.FromCentral.State != "up_to_date" || c.FromCentral.UpdateMode != "auto" {
		t.Errorf("set to automatic while behind: %s %+v", dayList(c), c.FromCentral)
	}
}

// CAL-27: a catalogue calendar made global (a freeze) is not applied to a
// copy its agency keeps to (binds as only), in either mode; the copy stays
// behind and says why.
func TestCA4a_AnUpdateTheAgencysBindingsForbidStaysBehind(t *testing.T) {
	f := newCatalogueFixture(t)
	exec := mustExec(t, f.pool)
	uid := f.catalogue("workdays", "2026-12-21")
	for _, a := range []struct{ agency, who, mode string }{{"ag:FIN", gFinAdmin, "auto"}, {"ag:TAX", gTaxAdmin, "ask"}} {
		if rec := f.in(a.agency, a.who, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`","updateMode":"`+a.mode+`"}`); rec.Code != http.StatusCreated {
			t.Fatalf("add to %s = %d (%s)", a.agency, rec.Code, rec.Body)
		}
		exec(`INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency, only_calendars) VALUES (?, ?, 'cronomicon', '0 2 * * *', 'h', ?, '["workdays"]')`,
			"s-"+a.agency, "keeps-"+a.agency, a.agency)
	}
	if rec := gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/workdays?agencyId=global", gRoot, `{"global":true}`); rec.Code != http.StatusOK {
		t.Fatalf("making the catalogue calendar a freeze = %d (%s)", rec.Code, rec.Body)
	}
	for _, a := range []string{"ag:FIN", "ag:TAX"} {
		c := f.get(a, "workdays")
		if c.Global || c.FromCentral.State != "update_available" || !strings.Contains(c.FromCentral.Blocked, "keeps-"+a) {
			t.Errorf("%s's copy = global %v %+v, want behind, blocked by its schedule", a, c.Global, c.FromCentral)
		}
		_, detail, open := f.openNotice("calendar_update_available", "calendar:"+c.UID)
		if !open || !strings.Contains(detail, "keeps-"+a) {
			t.Errorf("%s's notice = %q (open %v), want the reason", a, detail, open)
		}
	}
	if rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/workdays/update?agencyId=ag:TAX", gTaxAdmin, ""); rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "calendar_update_blocked" {
		t.Errorf("TAX's Update = %d %s, want 409 calendar_update_blocked", rec.Code, rec.Body)
	}
	rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/catalogue/"+uid+"/push", gRoot, `{"agencyIds":["ag:TAX"]}`)
	if !strings.Contains(rec.Body.String(), `"blocked":[{"agencyId":"ag:TAX","name":"workdays","reason":`) {
		t.Errorf("the push to TAX = %s, want it listed as blocked with the reason", rec.Body)
	}
}

func TestCA4a_ACatalogueCalendarDeletedLeavesItsCopies(t *testing.T) {
	f := newCatalogueFixture(t)
	uid := f.catalogue("holidays", "2026-12-25")
	if rec := f.in("ag:FIN", gFinAdmin, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	copyUID := f.get("ag:FIN", "holidays").UID
	f.setDays("global", "holidays", "2026-12-25", "2026-12-26") // behind, then gone
	if rec := gateReq(t, f.h, http.MethodDelete, "/api/v1/calendars/holidays?agencyId=global", gRoot, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("deleting the catalogue calendar = %d (%s)", rec.Code, rec.Body)
	}
	c := f.get("ag:FIN", "holidays")
	if c.FromCentral == nil || c.FromCentral.State != "no_longer_offered" || dayList(c) != "2026-12-25" {
		t.Errorf("the copy after the delete = %s %+v, want kept, no longer offered", dayList(c), c.FromCentral)
	}
	if _, _, open := f.openNotice("calendar_no_longer_offered", "calendar:"+copyUID); !open {
		t.Error("no calendar_no_longer_offered notice")
	}
	if _, _, open := f.openNotice("calendar_update_available", "calendar:"+copyUID); open {
		t.Error("the update notice outlived the catalogue calendar")
	}
	if code := gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/holidays/days?agencyId=ag:FIN", gFinAdmin, `{"days":[]}`).Code; code != http.StatusConflict {
		t.Errorf("editing a copy no longer offered = %d, want 409", code)
	}
	if rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/holidays/update?agencyId=ag:FIN", gFinAdmin, ""); errCode(rec.Body.Bytes()) != "calendar_no_longer_offered" {
		t.Errorf("updating it = %d %s", rec.Code, rec.Body)
	}
	// Made again under the same name, it is another calendar: the old copy
	// does not re-attach.
	f.catalogue("holidays", "2026-12-25")
	if c := f.get("ag:FIN", "holidays"); c.FromCentral.State != "no_longer_offered" {
		t.Errorf("the copy re-attached to a new catalogue calendar of the name: %+v", c.FromCentral)
	}
	if rec := gateReq(t, f.h, http.MethodPost, "/api/v1/calendars/holidays/detach?agencyId=ag:FIN", gFinAdmin, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if _, _, open := f.openNotice("calendar_no_longer_offered", "calendar:"+copyUID); open {
		t.Error("the notice is open after the copy was made its own")
	}
}

// CA-25: no run resolves a catalogue calendar. An agency's entry naming one
// finds none of its agency's, and a freeze calendar of Central's freezes no
// agency.
func TestCA4a_NoRunResolvesACatalogueCalendar(t *testing.T) {
	f := newCatalogueFixture(t)
	today := time.Now().UTC().Format("2006-01-02")
	f.catalogue("freeze", today)
	if rec := gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/freeze?agencyId=global", gRoot, `{"global":true}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	set, err := calendar.Load(context.Background(), f.pool, "ag:FIN", []string{"freeze"})
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 0 {
		t.Errorf("FIN's runs resolve %v, want none of Central's", set)
	}
	if v := calendar.EvaluateFor(set, "ag:FIN", []string{"freeze"}, nil, today); v.Suppressed {
		t.Errorf("a FIN entry skipping freeze is suppressed by Central's: %+v", v)
	}
}

// The review of CA4a: a change undone in the catalogue leaves the copy level
// again, and its notice resolved; a save that changes nothing raises nothing;
// one agency may hold two copies of one catalogue calendar, each followed.
func TestCA4a_ACopyLevelAgainHasNoNotice(t *testing.T) {
	f := newCatalogueFixture(t)
	uid := f.catalogue("holidays", "2026-12-25")
	for _, name := range []string{"holidays", "holidays-ops"} {
		if rec := f.in("ag:FIN", gFinAdmin, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`","name":"`+name+`"}`); rec.Code != http.StatusCreated {
			t.Fatalf("add %s = %d (%s)", name, rec.Code, rec.Body)
		}
	}
	f.setDays("global", "holidays", "2026-12-25", "2026-12-26")
	if n := count(t, f.pool, `SELECT COUNT(*) FROM notices WHERE kind = 'calendar_update_available' AND resolved_at IS NULL AND agency_id = 'ag:FIN'`); n != 2 {
		t.Fatalf("%d open notices for FIN's two copies, want 2", n)
	}
	seen := func() string {
		var s string
		_ = f.pool.QueryRow(`SELECT group_concat(last_seen_at) FROM notices WHERE kind = 'calendar_update_available'`).Scan(&s)
		return s
	}
	before := seen()
	time.Sleep(1100 * time.Millisecond)
	f.setDays("global", "holidays", "2026-12-26", "2026-12-25") // the same days: no change
	if seen() != before {
		t.Error("a save that changed nothing wrote the notices again")
	}
	f.setDays("global", "holidays", "2026-12-25") // undone
	for _, name := range []string{"holidays", "holidays-ops"} {
		if c := f.get("ag:FIN", name); c.FromCentral.State != "up_to_date" {
			t.Errorf("%s after the change was undone: %+v", name, c.FromCentral)
		}
	}
	if n := count(t, f.pool, `SELECT COUNT(*) FROM notices WHERE kind = 'calendar_update_available' AND resolved_at IS NULL`); n != 0 {
		t.Errorf("%d notices open after the change was undone, want none", n)
	}
}

// The catalogue's messages name the built-in agency as it is called now (2.5.2
// GR), never a literal: CA4a wrote "Central" into them, which an installation
// that kept Global, or a new one (Administration since 2.5.3), does not call it.
func TestCA4a_TheCatalogueSpeaksOfTheBuiltInAgencyByItsName(t *testing.T) {
	f := newCatalogueFixture(t)
	if _, err := f.pool.Exec(`UPDATE agencies SET name = 'Head Office' WHERE id = 'global'`); err != nil {
		t.Fatal(err)
	}
	uid := f.catalogue("holidays", "2026-12-25")
	if rec := f.in("ag:FIN", gFinAdmin, http.MethodPost, "/api/v1/calendars/from-central", `{"catalogueUid":"`+uid+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d (%s)", rec.Code, rec.Body)
	}
	rec := gateReq(t, f.h, http.MethodPut, "/api/v1/calendars/holidays?agencyId=ag:FIN", gFinAdmin, `{"description":"ours"}`)
	if body := rec.Body.String(); errCode(rec.Body.Bytes()) != "calendar_from_central" || !strings.Contains(body, "from Head Office") || strings.Contains(body, "Central") {
		t.Errorf("editing a copy = %d %s, want the refusal to name Head Office", rec.Code, body)
	}
	if rec := f.setDays("global", "holidays", "2026-12-25", "2026-12-26"); rec.Code != http.StatusOK {
		t.Fatalf("change the catalogue's calendar = %d (%s)", rec.Code, rec.Body)
	}
	if _, detail, open := f.openNotice("calendar_update_available", "calendar:"+f.get("ag:FIN", "holidays").UID); !open || !strings.HasPrefix(detail, "Head Office has changed holidays") {
		t.Errorf("the update notice (open %v) = %q, want it to name Head Office", open, detail)
	}
}
