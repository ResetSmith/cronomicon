package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// A scope whose inventory is in an agency's repository belongs to that agency
// (2.4.0, GR-18): each of the three routes that write a scope's agency answers
// 409 `scope_agency_fixed`, for a global administrator too, and writes nothing.
func TestAScopeOfAnAgencysRepositoryCannotBeMoved(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag:FIN', 'https://git.example/fin.git', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at, repo_id) VALUES ('sc:repo', 'fin-from-git', 'git', '2026-01-01T00:00:00Z', 'repo-fin')`)
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:repo' AND agency_id = 'ag:FIN'`); n != 1 {
		t.Fatalf("the scope of FIN's repository was not born FIN's")
	}
	for _, c := range []struct{ path, body string }{
		{"/api/v1/scopes/sc:repo/agency", `{"agencyId":"ag:TAX"}`},
		{"/api/v1/scopes/sc:repo/agency", `{"agencyId":null}`},
		{"/api/v1/scope-agencies", `[{"id":"sc:repo","agencyIds":["ag:TAX"]}]`},
		{"/api/v1/agencies/ag:TAX/members", `{"members":[{"kind":"scope","id":"sc:tax"},{"kind":"scope","id":"sc:repo"}]}`},
	} {
		rec := gateReq(t, h, http.MethodPut, c.path, gRoot, c.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "scope_agency_fixed") {
			t.Errorf("PUT %s %s = %d, want 409 scope_agency_fixed (%s)", c.path, c.body, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:repo'`); n != 1 {
		t.Errorf("after the refused moves the scope has %d agency rows, want its one", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:repo' AND agency_id = 'ag:FIN'`); n != 1 {
		t.Errorf("after the refused moves the scope is no longer FIN's")
	}
	// Naming the agency it has is no move.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:repo/agency", gRoot, `{"agencyId":"ag:FIN"}`); rec.Code != http.StatusOK {
		t.Errorf("setting the scope to its own agency = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}
