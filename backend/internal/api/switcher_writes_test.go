package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
)

// Writes in an agency context (2.4.4 S3). What a person creates while working
// in an agency is that agency's: a create that names no owner takes it, and
// one that names another agency is refused, with nothing written. A request
// that names no agency is answered as before.
func TestSwitcherACreateInAnAgencyIsThatAgencys(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	finTax := gFinAdmin + "," + gTaxAdmin
	in := func(groups, agency, method, path, body string) (int, string) {
		t.Helper()
		var rec interface {
			Result() *http.Response
		}
		_ = rec
		r := gateReqWithHeader(t, h, method, path, groups, body, agencyctx.Header, agency)
		return r.Code, r.Body.String()
	}
	ownerOf := func(q string, args ...any) string {
		t.Helper()
		var o string
		if err := pool.QueryRow(q, args...).Scan(&o); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return o
	}

	// Before 2.4.4, and still with no agency named: a person of two agencies
	// must say which.
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-vars", finTax, `{"key":"S3_NONE","value":"v","scope":"fin-hosts"}`); r.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(r.Body.String(), "agency_required") {
		t.Fatalf("a variable with no agency named, by a person of two = %d %s, want 422 agency_required", r.Code, r.Body)
	}
	// Working in FIN, they need not.
	if c, b := in(finTax, "ag:FIN", http.MethodPost, "/api/v1/env-vars", `{"key":"S3_FIN","value":"v","scope":"fin-hosts"}`); c != http.StatusCreated {
		t.Fatalf("a variable created in FIN = %d %s, want 201", c, b)
	}
	if o := ownerOf(`SELECT owner_agency FROM env_vars WHERE key = 'S3_FIN'`); o != "ag:FIN" {
		t.Errorf("the variable created in FIN is %q's", o)
	}
	// Naming TAX while working in FIN is refused, and nothing is written.
	if c, b := in(finTax, "ag:FIN", http.MethodPost, "/api/v1/env-vars", `{"key":"S3_TAX","value":"v","scope":"tax-hosts","agencyIds":["ag:TAX"]}`); c != http.StatusUnprocessableEntity ||
		!strings.Contains(b, "agency_context_mismatch") {
		t.Fatalf("a variable for TAX created in FIN = %d %s, want 422 agency_context_mismatch", c, b)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM env_vars WHERE key = 'S3_TAX'`).Scan(&n)
	if n != 0 {
		t.Errorf("the refused variable was written")
	}
	// Naming an agency one may not read is a refusal, not a create elsewhere.
	if c, b := in(gFinAdmin, "ag:TAX", http.MethodPost, "/api/v1/env-vars", `{"key":"S3_X","value":"v","scope":"fin-hosts"}`); c != http.StatusUnprocessableEntity ||
		!strings.Contains(b, "agency_context_mismatch") {
		t.Fatalf("a variable created by FIN's admin while naming TAX = %d %s, want 422 agency_context_mismatch", c, b)
	}

	// A calendar and a scope, by a global administrator working in FIN.
	if c, b := in(gRoot, "ag:FIN", http.MethodPost, "/api/v1/calendars", `{"name":"s3-cal","days":[{"day":"2026-12-25"}]}`); c != http.StatusCreated {
		t.Fatalf("a calendar created in FIN = %d %s, want 201", c, b)
	}
	if o := ownerOf(`SELECT owner_agency FROM calendars WHERE name = 's3-cal'`); o != "ag:FIN" {
		t.Errorf("the calendar created in FIN is %q's", o)
	}
	if c, b := in(gRoot, "ag:FIN", http.MethodPost, "/api/v1/scopes", `{"scope":"s3-scope","hosts":["h1"]}`); c != http.StatusCreated {
		t.Fatalf("a scope created in FIN = %d %s, want 201", c, b)
	}
	if o := ownerOf(`SELECT owner_agency FROM scopes WHERE name = 's3-scope'`); o != "ag:FIN" {
		t.Errorf("the scope created in FIN is %q's", o)
	}
	// Working in Global, a global administrator creates Global's, as before.
	if c, b := in(gRoot, "global", http.MethodPost, "/api/v1/calendars", `{"name":"s3-gcal","days":[{"day":"2026-12-25"}]}`); c != http.StatusCreated {
		t.Fatalf("a calendar created in Global = %d %s, want 201", c, b)
	}
	if o := ownerOf(`SELECT owner_agency FROM calendars WHERE name = 's3-gcal'`); o != "global" {
		t.Errorf("the calendar created in Global is %q's", o)
	}

	// A job with no scope, composed in FIN, is FIN's.
	if c, b := in(gRoot, "ag:FIN", http.MethodPost, "/api/v1/jobs", `{"name":"s3-job","scope":"","scriptRef":"s0-fin-script.sh"}`); c != http.StatusCreated {
		t.Fatalf("a job created in FIN = %d %s, want 201", c, b)
	}
	if o := ownerOf(`SELECT owner_agency FROM jobs WHERE name = 's3-job'`); o != "ag:FIN" {
		t.Errorf("the job created in FIN is %q's", o)
	}
	// A schedule composed in TAX is TAX's.
	if c, b := in(gRoot, "ag:TAX", http.MethodPost, "/api/v1/schedule-defs", `{"name":"s3-sched","cron":"0 5 * * *"}`); c != http.StatusCreated {
		t.Fatalf("a schedule created in TAX = %d %s, want 201", c, b)
	}
	if o := ownerOf(`SELECT owner_agency FROM schedules WHERE name = 's3-sched'`); o != "ag:TAX" {
		t.Errorf("the schedule created in TAX is %q's", o)
	}
}

// A global administrator's change made while working in an agency is audited
// with both (the 2.5.0 plan's IN-9): change_log records the agency beside the
// actor. Working in Global, or naming none, records nothing more.
func TestSwitcherAGlobalAdministratorsChangeInAnAgencyIsAuditedWithBoth(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	before := func() int {
		var n int
		_ = pool.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM change_log`).Scan(&n)
		return n
	}
	acting := func(after int) []string {
		t.Helper()
		rows, err := pool.Query(`SELECT DISTINCT COALESCE(acting_agency, '') FROM change_log WHERE rowid > ?`, after)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			out = append(out, a)
		}
		return out
	}
	mark := before()
	if r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/env-vars", gRoot, `{"key":"S3_A","value":"v"}`, agencyctx.Header, "ag:FIN"); r.Code != http.StatusCreated {
		t.Fatalf("create in FIN: %d %s", r.Code, r.Body)
	}
	if got := acting(mark); len(got) != 1 || got[0] != "ag:FIN" {
		t.Errorf("the global administrator's change in FIN is audited as acting in %v, want [ag:FIN]", got)
	}
	// A change in Global: until 2.5.3 a variable; since CA-4 Central owns no
	// variable (refused, nothing audited), and the change is a push target,
	// which it still owns (CA-2).
	mark = before()
	if r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/env-vars", gRoot, `{"key":"S3_B","value":"v"}`, agencyctx.Header, "global"); r.Code != http.StatusUnprocessableEntity || errCode(r.Body.Bytes()) != "central_runs_nothing" {
		t.Errorf("a variable created in Global: %d %s, want 422 central_runs_nothing", r.Code, r.Body)
	}
	if got := acting(mark); len(got) != 0 {
		t.Errorf("the refused variable was audited: %v", got)
	}
	if r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/alert-targets", gRoot, `{"label":"ops","url":"json://GLOBTOKEN"}`, agencyctx.Header, "global"); r.Code != http.StatusCreated {
		t.Fatalf("create in Global: %d %s", r.Code, r.Body)
	}
	if got := acting(mark); len(got) != 1 || got[0] != "" {
		t.Errorf("a change in Global records an acting agency: %v", got)
	}
	mark = before()
	if r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/env-vars", gFinAdmin, `{"key":"S3_C","value":"v","scope":"fin-hosts"}`, agencyctx.Header, "ag:FIN"); r.Code != http.StatusCreated {
		t.Fatalf("create by FIN's admin in FIN: %d %s", r.Code, r.Body)
	}
	if got := acting(mark); len(got) != 1 || got[0] != "" {
		t.Errorf("FIN's own administrator's change records an acting agency: %v", got)
	}
}
