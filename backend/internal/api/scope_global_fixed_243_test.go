package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The owner's answer to question 58 (2.4.3, option A): a scope whose inventory
// is in Global's repository is Global's, as one of an agency's repository is
// that agency's (GR-18). Until 2.4.3 a global administrator's repair gave one
// to an agency, which then held a scope Global's sync went on writing, could
// not edit, and lost when the file left Global's repository. Each route that
// writes a scope's agency now answers 409 scope_agency_fixed and writes
// nothing; the scope can be cloned to the agency instead, and the list says
// it cannot be repaired.
func TestRV_AScopeOfGlobalsRepositoryIsClonedNotRepaired(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scopes (id, name, source, created_at, repo_id) VALUES ('sc:g', 'global-web', 'git', '2026-01-01T00:00:00Z', 'global')`)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:g', 'h1')`)
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:g' AND agency_id = 'global'`); n != 1 {
		t.Fatalf("the scope of Global's repository was not born Global's")
	}
	for _, c := range []struct{ path, body string }{
		{"/api/v1/scopes/sc:g/agency", `{"agencyId":"ag:FIN"}`},
		{"/api/v1/scope-agencies", `[{"id":"sc:g","agencyIds":["ag:FIN"]}]`},
		{"/api/v1/agencies/ag:FIN/members", `{"members":[{"kind":"scope","id":"sc:fin"},{"kind":"scope","id":"sc:g"}]}`},
	} {
		rec := gateReq(t, h, http.MethodPut, c.path, gRoot, asRepair(c.body))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "scope_agency_fixed") {
			t.Errorf("PUT %s %s as a repair = %d, want 409 scope_agency_fixed (%s)", c.path, c.body, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:g'`); n != 1 {
		t.Errorf("after the refused repairs the scope has %d agency rows, want its one", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:g' AND agency_id = 'global'`); n != 1 {
		t.Errorf("after the refused repairs the scope is no longer Global's")
	}

	rec := gateReq(t, h, http.MethodGet, "/api/v1/scopes", gRoot, "")
	var rows []struct {
		ID        string `json:"id"`
		CanRepair bool   `json:"canRepair"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("the scope list: %v %s", err, rec.Body)
	}
	for _, row := range rows {
		if row.ID == "sc:g" && row.CanRepair {
			t.Errorf("the scope of Global's repository says it can be repaired")
		}
	}

	if rec := gateReq(t, h, http.MethodPost, "/api/v1/scopes/sc:g/clone", gRoot, `{"agencyId":"ag:FIN","name":"fin-web"}`); rec.Code != http.StatusCreated {
		t.Fatalf("cloning it to FIN = %d %s, want 201", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scopes s JOIN scope_hosts h ON h.scope_id = s.id
	                         WHERE s.name = 'fin-web' AND s.owner_agency = 'ag:FIN' AND s.source = 'cronomicon'`); n != 1 {
		t.Errorf("the clone is not FIN's own scope with the host (%d)", n)
	}
}
