package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Moves become repair (§0a, IN-7; Phase I4; the owner's answers to questions
// 42 to 45). On every route that can change an object's agency: an
// administrator of both agencies is refused (until I4 the two-sided rule let
// them move a secret, a variable, a key, a host record or a bastion); a
// global administrator is told the change is a repair (409 repair_required)
// unless the request says so, and needs a reason (422); the repair goes
// through and is audited with its reason; and a write that changes no agency
// is still the agency's own.
func TestI4_AMoveIsAGlobalAdministratorsRepairWithAReason(t *testing.T) {
	h, pool := gateServer(t)
	both := gFinAdmin + "," + gTaxAdmin

	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"DB_PASSWORD","source":"stored","scope":"fin-hosts","value":"v"}`)
	var sec struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &sec) != nil || sec.ID == "" {
		t.Fatalf("FIN creating a secret = %d (%s)", rec.Code, rec.Body)
	}

	for _, c := range []struct {
		what, path, body, target, where string
	}{
		{"a scope, on its own route", "/api/v1/scopes/sc:fin/agency", `{"agencyId":"ag:TAX"}`, "scope sc:fin",
			`SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:fin' AND agency_id = 'ag:TAX'`},
		{"a secret, on the matrix", "/api/v1/secret-agencies", `[{"id":"` + sec.ID + `","agencyIds":["ag:TAX"]}]`, "secret " + sec.ID,
			`SELECT COUNT(*) FROM secret_agencies WHERE secret_id = '` + sec.ID + `' AND agency_id = 'ag:TAX'`},
	} {
		// An administrator of both: refused, whether or not they ask for a repair.
		for _, body := range []string{c.body, asRepair(c.body)} {
			if rec := gateReq(t, h, http.MethodPut, c.path, both, body); rec.Code != http.StatusForbidden {
				t.Errorf("%s: an administrator of both agencies moving it = %d, want 403 (%s)", c.what, rec.Code, rec.Body)
			}
		}
		// A global administrator who does not say it is a repair, or gives no reason.
		if rec := gateReq(t, h, http.MethodPut, c.path, gRoot, c.body); rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "repair_required" {
			t.Errorf("%s: a global administrator moving it without a repair = %d %s, want 409 repair_required", c.what, rec.Code, rec.Body)
		}
		noReason := strings.Replace(asRepair(c.body), `"reason":"a test's repair"`, `"reason":"   "`, 1)
		if rec := gateReq(t, h, http.MethodPut, c.path, gRoot, noReason); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: a repair with no reason = %d, want 422 (%s)", c.what, rec.Code, rec.Body)
		}
		if n := count(t, pool, c.where); n != 0 {
			t.Fatalf("%s: a refused move moved it", c.what)
		}
		// The repair, with its reason in the audit log.
		reason := `"reason":"placed in FIN by mistake at the upgrade"`
		repair := strings.Replace(asRepair(c.body), `"reason":"a test's repair"`, reason, 1)
		if rec := gateReq(t, h, http.MethodPut, c.path, gRoot, repair); rec.Code != http.StatusOK {
			t.Fatalf("%s: a global administrator's repair = %d (%s)", c.what, rec.Code, rec.Body)
		}
		if n := count(t, pool, c.where); n != 1 {
			t.Errorf("%s: the repair did not move it", c.what)
		}
		if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE action = 'Repaired' AND target = ?
		                         AND details LIKE '%ag:TAX%' AND details LIKE '%placed in FIN by mistake at the upgrade%'`, c.target); n != 1 {
			t.Errorf("%s: the repair's audit row with its reason: %d, want 1", c.what, n)
		}
	}

	// A write that changes nothing is the agency's own, and not a repair.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:tax/agency", gTaxAdmin, `{"agencyId":"ag:TAX"}`); rec.Code != http.StatusOK {
		t.Errorf("TAX naming the agency its scope is in = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE action = 'Repaired' AND target = 'scope sc:tax'`); n != 0 {
		t.Errorf("a write that changed nothing was audited as a repair")
	}
}
