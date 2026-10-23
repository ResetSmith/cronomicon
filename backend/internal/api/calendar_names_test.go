package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// A calendar's name is unique within its agency (§0a, IN-5; Phase I2): two
// agencies each have `holidays`, each writes its own, a route by the name
// alone asks whose when both exist, and a job names its own agency's.
func TestI2_TwoAgenciesEachHaveTheirOwnCalendarOfOneName(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	code := func(who, method, path, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, method, path, who, body)
		return rec.Code, rec.Body.String()
	}
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/calendars", `{"name":"holidays","ownerAgency":"ag:FIN","days":[{"day":"2026-12-25"}]}`); c != http.StatusCreated {
		t.Fatalf("FIN creating its holidays = %d %s", c, b)
	}
	if c, b := code(gTaxAdmin, http.MethodPost, "/api/v1/calendars", `{"name":"holidays","ownerAgency":"ag:TAX","days":[{"day":"2026-07-04"},{"day":"2026-07-05"}]}`); c != http.StatusCreated {
		t.Fatalf("TAX creating its holidays beside FIN's = %d %s, want 201", c, b)
	}
	if c, _ := code(gTaxAdmin, http.MethodPost, "/api/v1/calendars", `{"name":"holidays","ownerAgency":"ag:TAX","days":[]}`); c != http.StatusConflict {
		t.Errorf("TAX creating a second holidays = %d, want 409", c)
	}

	// By the name alone the route cannot tell them apart; with the agency it can.
	if c, b := code(gRoot, http.MethodGet, "/api/v1/calendars/holidays", ""); c != http.StatusConflict || !strings.Contains(b, "ambiguous_calendar") {
		t.Errorf("reading holidays with two agencies holding it = %d %s, want 409 ambiguous_calendar", c, b)
	}
	if c, b := code(gRoot, http.MethodGet, "/api/v1/calendars/holidays?agencyId=ag:TAX", ""); c != http.StatusOK || !strings.Contains(b, `"ownerAgency":"ag:TAX"`) || !strings.Contains(b, "2026-07-05") {
		t.Errorf("reading TAX's holidays = %d %s, want TAX's, with its days", c, b)
	}
	if c, _ := code(gRoot, http.MethodGet, "/api/v1/calendars/holidays?agencyId=ag:NONE", ""); c != http.StatusNotFound {
		t.Errorf("reading holidays in an agency that has none = %d, want 404", c)
	}

	// Each agency writes its own, and not the other's.
	if c, _ := code(gTaxAdmin, http.MethodPut, "/api/v1/calendars/holidays/days?agencyId=ag:TAX", `{"days":[{"day":"2026-07-04"}]}`); c != http.StatusOK {
		t.Errorf("TAX replacing its holidays' days = %d, want 200", c)
	}
	if c, _ := code(gTaxAdmin, http.MethodPut, "/api/v1/calendars/holidays/days?agencyId=ag:FIN", `{"days":[]}`); c != http.StatusNotFound {
		t.Errorf("TAX replacing FIN's holidays' days = %d, want 404 (question 72: not TAX's to read)", c)
	}
	if c, b := code(gFinAdmin, http.MethodGet, "/api/v1/calendars/holidays?agencyId=ag:FIN", ""); c != http.StatusOK || !strings.Contains(b, "2026-12-25") {
		t.Errorf("FIN's holidays after TAX's edit = %d %s, want its own day kept", c, b)
	}

	// A job names its own agency's calendar of the name, and not a calendar
	// only another agency has.
	body := `{"name":"fin-cal-job","scope":"fin-hosts","scriptRef":"backup-db","schedules":[{"name":"nightly","cron":"0 2 * * *","skipCalendars":["holidays"]}]}`
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", body); c/100 != 2 {
		t.Errorf("FIN's job naming holidays = %d %s, want 2xx", c, b)
	}
	if c, b := code(gTaxAdmin, http.MethodPost, "/api/v1/calendars", `{"name":"tax-only","ownerAgency":"ag:TAX","days":[{"day":"2026-08-01"}]}`); c != http.StatusCreated {
		t.Fatalf("TAX creating tax-only = %d %s", c, b)
	}
	body = `{"name":"fin-cal-job-2","scope":"fin-hosts","scriptRef":"backup-db","schedules":[{"name":"nightly","cron":"0 2 * * *","skipCalendars":["tax-only"]}]}`
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", body); c != http.StatusUnprocessableEntity {
		t.Errorf("FIN's job naming a calendar only TAX has = %d %s, want 422", c, b)
	}

	// Deleting TAX's leaves FIN's, and the name is unambiguous again.
	if c, _ := code(gTaxAdmin, http.MethodDelete, "/api/v1/calendars/holidays?agencyId=ag:TAX", ""); c != http.StatusNoContent {
		t.Errorf("TAX deleting its holidays = %d, want 204", c)
	}
	if c, b := code(gRoot, http.MethodGet, "/api/v1/calendars/holidays", ""); c != http.StatusOK || !strings.Contains(b, `"ownerAgency":"ag:FIN"`) {
		t.Errorf("reading holidays with one agency holding it = %d %s, want FIN's", c, b)
	}
}
