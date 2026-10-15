package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The lists carry, per row, what the caller may do with it (2.4.0 Phase R8;
// the owner's answer to question 49), and each flag says what the row's own
// route answers: for every row of the scope, host record, bastion and agency
// lists, as an agency's administrator and as a global administrator, the
// row's DELETE is made and its status compared with canDelete. (Delete is
// asked rather than edit: it needs no body, and every one of these routes has
// the same gate for both.) Schedule rows say whose they are.
func TestR8_EachRowsFlagsSayWhatItsRouteAnswers(t *testing.T) {
	type row struct {
		ID        string
		Name      string
		CanEdit   bool
		CanDelete bool
		CanRepair bool
	}
	for _, who := range []string{gFinAdmin, gRoot} {
		h, pool := gateServer(t)
		exec := mustExec(t, pool)
		exec(`INSERT INTO ssh_hosts (id, hostname, port, source, status, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		      VALUES ('h-fin', 'fin01', 22, 'cronomicon', 'unknown', 't', 't', 't', 't', 'ag:FIN'),
		             ('h-tax', 'tax01', 22, 'cronomicon', 'unknown', 't', 't', 't', 't', 'ag:TAX')`)
		exec(`INSERT INTO bastions (id, hostname, name, address, port, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		      VALUES ('b-fin', 'jump-fin', 'bf', '10.0.0.1', 22, 't', 't', 't', 't', 'ag:FIN'),
		             ('b-glob', 'jump-glob', 'bg', '10.0.0.2', 22, 't', 't', 't', 't', 'global')`)
		global := who == gRoot

		for _, c := range []struct{ list, del string }{
			{"/api/v1/scopes", "/api/v1/scopes/"},
			{"/api/v1/ssh/hosts", "/api/v1/ssh/hosts/"},
			{"/api/v1/ssh/bastions", "/api/v1/ssh/bastions/"},
			{"/api/v1/agencies", "/api/v1/agencies/"},
		} {
			rec := gateReq(t, h, http.MethodGet, c.list, who, "")
			var rows []row
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rows) != nil || len(rows) == 0 {
				t.Fatalf("%s %s = %d (%s)", who, c.list, rec.Code, rec.Body)
			}
			for _, r := range rows {
				if r.CanRepair != global {
					t.Errorf("%s %s %s: canRepair = %v, want %v", who, c.list, r.ID, r.CanRepair, global)
				}
				if r.CanEdit != r.CanDelete {
					t.Errorf("%s %s %s: canEdit %v and canDelete %v differ, and their routes have one gate", who, c.list, r.ID, r.CanEdit, r.CanDelete)
				}
				code := gateReq(t, h, http.MethodDelete, c.del+r.ID, who, "").Code
				allowed := code != http.StatusForbidden && code != http.StatusUnprocessableEntity // 422: Global, the built-in agency
				if r.CanDelete != allowed {
					t.Errorf("%s %s %s: canDelete = %v, but its DELETE answered %d", who, c.list, r.ID, r.CanDelete, code)
				}
			}
		}
	}

	// A schedule row says whose it is.
	h, pool := gateServer(t)
	mustExec(t, pool)(`INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency) VALUES ('s-fin', 'fin-nightly', 'cronomicon', '0 2 * * *', 'h', 'ag:FIN')`)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/schedule-defs?source=cronomicon", gRoot, "")
	var page struct {
		Items []struct{ Name, OwnerAgency string }
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
		t.Fatalf("listing schedules = %d (%s)", rec.Code, rec.Body)
	}
	found := false
	for _, s := range page.Items {
		if s.Name == "fin-nightly" {
			found = true
			if s.OwnerAgency != "ag:FIN" {
				t.Errorf("fin-nightly's ownerAgency = %q, want ag:FIN", s.OwnerAgency)
			}
		}
	}
	if !found {
		t.Errorf("fin-nightly is not in the list: %s", rec.Body)
	}
}
