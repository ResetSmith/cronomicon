package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The agency's contact (2.5.2 OP1.1, OP-28): who the central team calls about
// an agency's runs when the job or workflow names no one. Its administrators
// or a global administrator set it; the overview and the run details fall back
// to it.

func TestOP1AnAgencysContactIsItsAdministratorsToSet(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	put := func(agency, groups, body string) int {
		t.Helper()
		return gateReq(t, h, http.MethodPut, "/api/v1/agencies/"+agency+"/contact", groups, body).Code
	}
	for _, c := range []struct {
		who, groups, agency string
		want                int
	}{
		{"FIN's admin on FIN", gFinAdmin, "ag:FIN", http.StatusOK},
		{"FIN's admin on TAX", gFinAdmin, "ag:TAX", http.StatusForbidden},
		{"FIN's admin on Global", gFinAdmin, "global", http.StatusForbidden},
		{"a viewer of everything", gAllViewer, "ag:FIN", http.StatusForbidden},
		{"a global administrator on TAX", gRoot, "ag:TAX", http.StatusOK},
		{"a global administrator on Global", gRoot, "global", http.StatusOK},
	} {
		if got := put(c.agency, c.groups, `{"contact":"  on-call, ext. 4410  "}`); got != c.want {
			t.Errorf("%s = %d, want %d", c.who, got, c.want)
		}
	}

	rec := gateReq(t, h, http.MethodGet, "/api/v1/agencies", gRoot, "")
	var list []struct {
		ID      string `json:"id"`
		Contact string `json:"contact"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("agencies: %d %s", rec.Code, rec.Body)
	}
	for _, a := range list {
		if a.ID == "ag:FIN" && a.Contact != "on-call, ext. 4410" {
			t.Errorf("FIN's contact %q, want it trimmed", a.Contact)
		}
	}

	// Recorded once, on the agency's own row of the change log; a repeat
	// changes nothing and records nothing.
	if got := put("ag:FIN", gFinAdmin, `{"contact":"on-call, ext. 4410"}`); got != http.StatusOK {
		t.Fatalf("repeat = %d", got)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action = 'contact-set' AND agency_id = 'ag:FIN'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("FIN's contact-set rows = %d (%v), want 1", n, err)
	}

	if got := put("ag:FIN", gFinAdmin, `{}`); got != http.StatusUnprocessableEntity {
		t.Errorf("a body without the field = %d, want 422 (it must not clear it)", got)
	}
	if got := put("ag:FIN", gFinAdmin, `{"contact":"`+strings.Repeat("x", 257)+`"}`); got != http.StatusUnprocessableEntity {
		t.Errorf("257 characters = %d, want 422", got)
	}
	if got := put("ag:NOPE", gRoot, `{"contact":"x"}`); got != http.StatusNotFound {
		t.Errorf("an unknown agency = %d, want 404", got)
	}
	if got := put("ag:FIN", gFinAdmin, `{"contact":""}`); got != http.StatusOK {
		t.Errorf("clearing = %d, want 200", got)
	}
	var contact string
	if err := pool.QueryRow(`SELECT contact FROM agencies WHERE id = 'ag:FIN'`).Scan(&contact); err != nil || contact != "" {
		t.Errorf("FIN's contact after clearing = %q (%v)", contact, err)
	}
}

func TestOP1TheAgencysContactIsTheFallback(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	seedOverview(t, h, exec)
	exec(`UPDATE agencies SET contact = 'FIN on-call' WHERE id = 'ag:FIN'`)
	exec(`UPDATE agencies SET contact = 'TAX desk' WHERE id = 'ag:TAX'`)
	// FIN's job names its own contact; TAX's names none.
	exec(`INSERT INTO annotations (owner_kind, owner_uid, critical, contact) VALUES ('job', 'j-fin', 0, 'fin job owner')`)

	rec := gateReq(t, h, http.MethodGet, "/api/v1/overview", gAllViewer, "")
	var ov struct {
		Attention []struct {
			Kind          string `json:"kind"`
			Name          string `json:"name"`
			Contact       string `json:"contact"`
			AgencyContact string `json:"agencyContact"`
		} `json:"attention"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ov) != nil {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	got := map[string][2]string{}
	for _, it := range ov.Attention {
		k := it.Kind + " " + it.Name
		if it.Kind == "sync_failed" {
			k = it.Kind // TAX's repository, the one failed sync seeded
		}
		got[k] = [2]string{it.Contact, it.AgencyContact}
	}
	for k, want := range map[string][2]string{
		"run_lost s0-fin-job":   {"fin job owner", ""},
		"job_failed s0-tax-job": {"", "TAX desk"},
		"sync_failed":           {"", "TAX desk"},
	} {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("%s: contact/agencyContact %q (listed %v), want %q", k, g, ok, want)
		}
	}

	// The run details: TAX's job names no one, so its agency's contact shows.
	var run struct {
		Annotation *struct {
			Contact       string `json:"contact"`
			AgencyContact string `json:"agencyContact"`
		} `json:"annotation"`
	}
	r := gateReq(t, h, http.MethodGet, "/api/v1/runs/r-tax", gRoot, "")
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &run) != nil {
		t.Fatalf("run: %d %s", r.Code, r.Body)
	}
	if run.Annotation == nil || run.Annotation.Contact != "" || run.Annotation.AgencyContact != "TAX desk" {
		t.Errorf("r-tax annotation %+v, want the agency's contact", run.Annotation)
	}
	// FIN's job names its own, so the agency's is not added.
	run.Annotation = nil
	r = gateReq(t, h, http.MethodGet, "/api/v1/runs/r-fin", gRoot, "")
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &run) != nil {
		t.Fatalf("run: %d %s", r.Code, r.Body)
	}
	if run.Annotation == nil || run.Annotation.Contact != "fin job owner" || run.Annotation.AgencyContact != "" {
		t.Errorf("r-fin annotation %+v, want the job's contact alone", run.Annotation)
	}
	run.Annotation = nil
	r = gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/wr-tax", gRoot, "")
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &run) != nil {
		t.Fatalf("workflow run: %d %s", r.Code, r.Body)
	}
	if run.Annotation == nil || run.Annotation.AgencyContact != "TAX desk" {
		t.Errorf("wr-tax annotation %+v, want the agency's contact", run.Annotation)
	}
}
