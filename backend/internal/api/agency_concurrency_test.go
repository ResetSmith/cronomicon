package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2.5.0 C6 (QT-1) — an agency's own concurrency cap: set by a global
// administrator, read on the agency's row, and named in the refusal of a run
// it holds.

func TestAnAgencysConcurrencyCapIsAGlobalAdministrators(t *testing.T) {
	h, _ := gateServer(t)
	put := func(groups, body string) (int, string) {
		rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/concurrency", groups, body)
		return rec.Code, rec.Body.String()
	}
	if code, body := put(gFinAdmin, `{"maxConcurrent":2}`); code != http.StatusForbidden {
		t.Errorf("FIN's own administrator setting FIN's cap = %d, want 403 (%s)", code, body)
	}
	code, body := put(gRoot, `{"maxConcurrent":2}`)
	if code != http.StatusOK {
		t.Fatalf("a global administrator setting FIN's cap = %d, want 200 (%s)", code, body)
	}
	var a struct {
		MaxConcurrent *int `json:"maxConcurrent"`
	}
	_ = json.Unmarshal([]byte(body), &a)
	if a.MaxConcurrent == nil || *a.MaxConcurrent != 2 {
		t.Errorf("the answer's maxConcurrent = %v, want 2", a.MaxConcurrent)
	}

	// The list carries it, so the agency's row can show it.
	rec := gateReq(t, h, http.MethodGet, "/api/v1/agencies", gRoot, "")
	var list []struct {
		ID            string `json:"id"`
		MaxConcurrent *int   `json:"maxConcurrent"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	for _, ag := range list {
		if ag.ID == "ag:FIN" && (ag.MaxConcurrent == nil || *ag.MaxConcurrent != 2) {
			t.Errorf("GET /agencies shows FIN's cap as %v, want 2", ag.MaxConcurrent)
		}
		if ag.ID == "ag:TAX" && ag.MaxConcurrent != nil {
			t.Errorf("TAX, which has none, shows a cap of %d", *ag.MaxConcurrent)
		}
	}

	for _, c := range []struct{ body, why string }{
		{`{"maxConcurrent":0}`, "zero"},
		{`{"maxConcurrent":-1}`, "negative"},
		{`{"maxConcurrent":"x"}`, "not a number"},
		{`{}`, "absent (must not read as none)"},
	} {
		if code, body := put(gRoot, c.body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422 (%s)", c.why, code, body)
		}
	}
	if code, body := put(gRoot, `{"maxConcurrent":null}`); code != http.StatusOK || !strings.Contains(body, `"maxConcurrent":null`) {
		t.Errorf("null removes the cap: %d %s", code, body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/global/concurrency", gRoot, `{"maxConcurrent":4}`); rec.Code != http.StatusOK {
		t.Errorf("Global's cap = %d, want 200: Global's runs are runs (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:NOPE/concurrency", gRoot, `{"maxConcurrent":4}`); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown agency = %d, want 404", rec.Code)
	}
}

func TestARunHeldByItsAgencysCapIsRefusedNamingTheAgency(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:fin','fin01')`)
	// A uid, as every writer of jobs gives one: the producer reads the job's
	// agency by it (a job with none would be read as Global's).
	exec(`INSERT INTO jobs (uid,name,source,run_type,scope,command,target_host,enabled,created_at) VALUES
	      ('uid-fin-job','fin-job','cronomicon','bash','fin-hosts','true','fin01',1,'2026-01-01T00:00:00Z')`)
	exec(`UPDATE agencies SET max_concurrent = 1 WHERE id = 'ag:FIN'`)
	id := rowID(t, pool, `SELECT rowid FROM jobs WHERE name=?`, "fin-job")
	run := func() *httptest.ResponseRecorder {
		return gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id+"/run", gFinOperator, `{}`)
	}

	if rec := run(); rec.Code/100 != 2 {
		t.Fatalf("FIN's first run = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
	rec := run()
	if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "concurrency_cap" {
		t.Fatalf("FIN's second run under a cap of 1 = %d %s, want 409 concurrency_cap", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "FIN's concurrency cap reached") {
		t.Errorf("the refusal does not name FIN's cap: %s", rec.Body)
	}
}
