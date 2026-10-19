package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// A calendar is its agency's (§0a, 2.4.0; SO-1): writing it takes compose on
// its owner, an entry binds its own agency's calendars at a write, and the
// installation-wide freeze is a global administrator's setting with a reason.
// Until §0a every calendar was Global's, an installation administrator's to
// write, any entry bound any, and Global's global calendar froze every agency.
func TestI1_ACalendarIsItsAgencys(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	code := func(who, method, path, body string) int {
		t.Helper()
		return gateReq(t, h, method, path, who, body).Code
	}
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/calendars", `{"name":"fin-holidays","ownerAgency":"ag:FIN","days":[{"day":"2026-12-25"}]}`); c != http.StatusCreated {
		t.Fatalf("FIN creating its calendar = %d, want 201", c)
	}
	if c := code(gTaxAdmin, http.MethodPut, "/api/v1/calendars/fin-holidays/days", `{"days":[]}`); c != http.StatusNotFound {
		t.Errorf("TAX replacing FIN's calendar's days = %d, want 404 (question 72: not TAX's to read)", c)
	}
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/calendars", `{"name":"for-global"}`); c != http.StatusForbidden {
		t.Errorf("FIN creating Global's calendar = %d, want 403", c)
	}
	if c := code(gRoot, http.MethodPost, "/api/v1/calendars", `{"name":"g-holidays","days":[{"day":"2026-12-25"}]}`); c != http.StatusCreated {
		t.Fatalf("a global administrator creating Global's calendar = %d, want 201", c)
	}
	job := func(name, cal string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"fin-hosts","schedules":[{"name":"n","cron":"0 2 * * *","skipCalendars":["` + cal + `"]}]}`
	}
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", job("fin-own-cal", "fin-holidays")); c/100 != 2 {
		t.Errorf("FIN's job naming FIN's calendar = %d, want 2xx", c)
	}
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", job("fin-global-cal", "g-holidays")); c != http.StatusUnprocessableEntity {
		t.Errorf("FIN's job naming Global's calendar = %d, want 422", c)
	}

	// The installation-wide freeze.
	if c := code(gFinAdmin, http.MethodPut, "/api/v1/settings/freeze", `{"on":true,"reason":"audit"}`); c != http.StatusForbidden {
		t.Errorf("FIN freezing the installation = %d, want 403", c)
	}
	if c := code(gRoot, http.MethodPut, "/api/v1/settings/freeze", `{"on":true}`); c != http.StatusUnprocessableEntity {
		t.Errorf("a freeze with no reason = %d, want 422", c)
	}
	if c := code(gRoot, http.MethodPut, "/api/v1/settings/freeze", `{"on":true,"reason":"year-end change freeze"}`); c != http.StatusOK {
		t.Fatalf("a global administrator freezing the installation = %d, want 200", c)
	}
	rec := gateReq(t, h, http.MethodGet, "/api/v1/settings/freeze", gFinAdmin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"on":true`) || !strings.Contains(rec.Body.String(), "year-end change freeze") {
		t.Errorf("reading the freeze = %d %s", rec.Code, rec.Body)
	}
}
