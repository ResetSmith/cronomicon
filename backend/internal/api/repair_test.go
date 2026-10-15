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

	// A reason too long, and the code of the refusal.
	long := strings.Repeat("x", 501)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:tax/agency", gRoot,
		`{"agencyId":"ag:FIN","repair":true,"reason":"`+long+`"}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "validation_failed" {
		t.Errorf("a repair with a reason of 501 characters = %d %s, want 422 validation_failed", rec.Code, rec.Body)
	}

	// A write that changes nothing is the agency's own, and not a repair.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:tax/agency", gTaxAdmin, `{"agencyId":"ag:TAX"}`); rec.Code != http.StatusOK {
		t.Errorf("TAX naming the agency its scope is in = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE action = 'Repaired' AND target = 'scope sc:tax'`); n != 0 {
		t.Errorf("a write that changed nothing was audited as a repair")
	}
}

// From the review of I4: what a write changes is the row's agencies AND its
// owner; a runner serving nobody is placed by a repair; a body that is not a
// list nor a wrapper is refused; what is not there is said before anything
// about repair; a bastion is repaired and audited like the rest; and a
// removal is audited as one.
func TestI4_ARepairIsAskedOfEveryChangeAndOnlyOfChanges(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)

	// A secret Global owns and FIN alone uses (what 1220 left): FIN posting
	// its membership as it stands would make FIN the owner.
	exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('s-g', 'SHARED_TOKEN', NULL, 'stored', 't', 'global')`)
	exec(`DELETE FROM secret_agencies WHERE secret_id = 's-g'`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-g', 'ag:FIN')`)
	claim := `[{"id":"s-g","agencyIds":["ag:FIN"]}]`
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gFinAdmin, claim); rec.Code != http.StatusForbidden {
		t.Errorf("FIN claiming the secret Global owns by posting its membership = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gRoot, claim); rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "repair_required" {
		t.Errorf("a global administrator changing its owner without a repair = %d %s, want 409 repair_required", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE id = 's-g' AND owner_agency = 'global'`); n != 1 {
		t.Fatal("a refused write changed the secret's owner")
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gRoot, asRepair(claim)); rec.Code != http.StatusOK {
		t.Errorf("the repair = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE id = 's-g' AND owner_agency = 'ag:FIN'`); n != 1 {
		t.Error("the repair did not make FIN the owner")
	}

	// A runner that is there and serves nobody (damage) is placed by a repair.
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('r-x', 'stray', 'offline', 't', 't', 'ag:FIN')`)
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-x'`)
	place := `[{"runnerId":"r-x","agencyIds":["ag:FIN"]}]`
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin, place); rec.Code != http.StatusForbidden {
		t.Errorf("FIN placing a runner that serves nobody = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_agencies WHERE runner_id = 'r-x'`); n != 0 {
		t.Error("a refused write placed the runner")
	}

	// A body that is a row, not a list, nor a wrapper with its rows: refused.
	for _, body := range []string{`{"id":"sc:fin","agencyIds":["ag:TAX"]}`, `{"repair":true,"reason":"x"}`} {
		if rec := gateReq(t, h, http.MethodPut, "/api/v1/scope-agencies", gRoot, body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("the body %s = %d, want 422 (%s)", body, rec.Code, rec.Body)
		}
	}

	// What is not there is said first, to a global administrator who has not
	// said it is a repair.
	for _, c := range []struct {
		what, path, body string
		want             int
	}{
		{"an unknown secret on the matrix", "/api/v1/secret-agencies", `[{"id":"nope","agencyIds":["ag:FIN"]}]`, http.StatusUnprocessableEntity},
		{"an unknown scope on its route", "/api/v1/scopes/nope/agency", `{"agencyId":"ag:FIN"}`, http.StatusNotFound},
		{"an unknown agency's members", "/api/v1/agencies/ag:NOPE/members", `{"members":[{"kind":"scope","id":"sc:fin"}]}`, http.StatusNotFound},
		{"an unknown member", "/api/v1/agencies/ag:FIN/members", `{"members":[{"kind":"scope","id":"sc:fin"},{"kind":"secret","id":"nope"}]}`, http.StatusUnprocessableEntity},
	} {
		if rec := gateReq(t, h, http.MethodPut, c.path, gRoot, c.body); rec.Code != c.want {
			t.Errorf("%s = %d, want %d (%s)", c.what, rec.Code, c.want, rec.Body)
		}
	}

	// A bastion is repaired, and audited with its reason.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/ssh/bastions", gFinAdmin, `{"name":"b","address":"10.0.0.1","port":22}`)
	var b struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &b) != nil || b.ID == "" {
		t.Fatalf("FIN creating a bastion = %d (%s)", rec.Code, rec.Body)
	}
	move := `{"name":"b","address":"10.0.0.1","port":22,"ownerAgency":"ag:TAX"}`
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/bastions/"+b.ID, gFinAdmin+","+gTaxAdmin, asRepair(move)); rec.Code != http.StatusForbidden {
		t.Errorf("an administrator of both agencies moving a bastion = %d, want 403", rec.Code)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/bastions/"+b.ID, gRoot, move); rec.Code != http.StatusConflict {
		t.Errorf("a global administrator moving a bastion without a repair = %d, want 409", rec.Code)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/bastions/"+b.ID, gRoot, asRepair(move)); rec.Code != http.StatusOK {
		t.Errorf("a bastion's repair = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE action = 'Repaired' AND target = ? AND details LIKE 'owner set to ag:TAX; reason: %'`, "bastion "+b.ID); n != 1 {
		t.Errorf("the bastion's repair audit row: %d, want 1", n)
	}

	// A removal on the agency editor is audited as one.
	exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('s-two', 'TWO', NULL, 'stored', 't', 'global')`)
	exec(`DELETE FROM secret_agencies WHERE secret_id = 's-two'`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-two', 'ag:FIN'), ('s-two', 'ag:TAX')`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot,
		asRepair(`{"members":[{"kind":"scope","id":"sc:fin"},{"kind":"secret","id":"s-g"}]}`)); rec.Code != http.StatusOK {
		t.Fatalf("a repair taking a shared secret out of FIN = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE action = 'Repaired' AND target = 'secret s-two' AND details LIKE 'removed from agency ag:FIN; reason: %'`); n != 1 {
		t.Errorf("the removal's audit row: %d, want 1", n)
	}
}
