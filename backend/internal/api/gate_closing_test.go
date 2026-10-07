package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Gate closing (GC band, v2.2.2).
//
// Each test below is a cross-agency act that answered 2xx before v2.2.2. They
// are written against the two-agency cast in gate_fixture_test.go, and most of
// them are a refusal: if one starts passing, an administrator of one agency
// can again reach the installation or another agency.

func rowID(t *testing.T, pool *sql.DB, q string, args ...any) string {
	t.Helper()
	var id int64
	if err := pool.QueryRow(q, args...).Scan(&id); err != nil {
		t.Fatalf("rowid: %v\n%s", err, q)
	}
	return strconv.FormatInt(id, 10)
}

func count(t *testing.T, pool *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v\n%s", err, q)
	}
	return n
}

// GC-3 — "unrestricted" must mean a grant that reaches everywhere AND carries
// the permission. gMixed reaches everywhere as a viewer and administers one
// agency; before v2.2.2 that passed both unrestricted-only guards.
func TestGC_UnrestrictedViewerWhoAdministersOneAgencyIsNotAGlobalAdmin(t *testing.T) {
	h, pool := gateServer(t)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/roles", gMixed,
		`{"name":"sneaky","description":"x","permissions":{"configureApp":true}}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mixed actor creating a role template = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPost, "/api/v1/access-grants", gMixed,
		`{"adGroup":"mine","role":"admin","agencyId":"","allScopes":true}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mixed actor minting an all-scopes grant = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM access_grants WHERE ad_group='mine'`); n != 0 {
		t.Errorf("an all-scopes grant was written by a non-global actor")
	}
	// The delegate half still works: the same actor grants inside FIN.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/access-grants", gMixed,
		`{"adGroup":"fin-ops","role":"operator","agencyId":"ag:FIN","allScopes":false}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("mixed actor granting operator in its own agency = %d, want 201 (%s)", rec.Code, rec.Body)
	}
}

// GC-4 — the shared authoring surfaces ask for compose AND configureApp on one
// unrestricted grant: not a role name, and not compose alone.
func TestGC_SharedAuthoringNeedsComposeAndConfigureOnEveryAgency(t *testing.T) {
	h, _ := gateServer(t)
	const body = `{"name":"cal-x","description":"","global":false,"days":[]}`
	for _, c := range []struct {
		who  string
		deny bool
		why  string
	}{
		{gFinAdmin, true, "the built-in admin role on ONE agency passed RequireRole(\"admin\")"},
		{gAllComposer, true, "compose on every agency, without configureApp, is not an installation administrator"},
		{gMixed, true, "compose and configureApp must come from the SAME unrestricted grant"},
		{gAllFull, false, "a custom role holding both on every agency is; the gate names no role"},
		{gRoot, false, "the built-in admin on every agency"},
	} {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/calendars", c.who, body)
		if got := rec.Code == http.StatusForbidden; got != c.deny {
			t.Errorf("%s creating a calendar = %d, want forbidden=%v — %s (%s)", c.who, rec.Code, c.deny, c.why, rec.Body)
		}
	}
}

// GC-5 — minting a service account is granting a role, under the grant's rules.
// These are the four requests reproduced on 2026-10-06, all of which were 201.
func TestGC_ServiceAccountsFollowTheDelegationRules(t *testing.T) {
	h, pool := gateServer(t)
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"own agency, a role it holds", `{"name":"p-own","role":"operator","agencyId":"ag:FIN","allScopes":false}`, http.StatusCreated},
		{"another agency", `{"name":"p-tax","role":"operator","agencyId":"ag:TAX","allScopes":false}`, http.StatusForbidden},
		{"every agency", `{"name":"p-all","role":"admin","agencyId":"","allScopes":true}`, http.StatusForbidden},
	} {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/service-accounts", gFinAdmin, c.body)
		if rec.Code != c.want {
			t.Errorf("fin admin minting for %s = %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
	}
	// Amplification: a delegate that may grant but does not hold configureApp
	// cannot mint a token whose role carries it, even in its own agency. The
	// role is created through the API so the process-wide role cache sees it.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/roles", gRoot,
		`{"name":"fin-delegate","description":"delegate","permissions":{"triggerJobs":true,"killJobs":true,"manageEnvVars":true,"publishSchedule":true,"configureApp":false,"manageRoles":true,"compose":true}}`); rec.Code/100 != 2 {
		t.Fatalf("root creating the delegate role = %d (%s)", rec.Code, rec.Body)
	}
	mustExec(t, pool)(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	      VALUES ('g:deleg','fin-delegates','fin-delegate','ag:FIN',0,'2026-01-01T00:00:00Z')`)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/service-accounts", "fin-delegates",
		`{"name":"p-amp","role":"admin","agencyId":"ag:FIN","allScopes":false}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("delegate minting a role carrying configureApp = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	if n := count(t, pool, `SELECT COUNT(*) FROM service_accounts`); n != 1 {
		t.Errorf("service accounts written = %d, want exactly the one in the caller's own agency", n)
	}

	// Revoking is administering. Root mints one for TAX; FIN may not revoke it,
	// and does not see it listed.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/service-accounts", gRoot,
		`{"name":"tax-bot","role":"operator","agencyId":"ag:TAX","allScopes":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("root minting for TAX = %d (%s)", rec.Code, rec.Body)
	}
	var minted struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/service-accounts/"+minted.ID, gFinAdmin, ""); rec.Code != http.StatusForbidden {
		t.Errorf("fin admin revoking TAX's account = %d, want 403", rec.Code)
	}
	list := gateReq(t, h, http.MethodGet, "/api/v1/service-accounts", gFinAdmin, "").Body.String()
	if strings.Contains(list, "tax-bot") || !strings.Contains(list, "p-own") {
		t.Errorf("fin admin's list must show FIN's accounts only, got %s", list)
	}
	if list := gateReq(t, h, http.MethodGet, "/api/v1/service-accounts", gRoot, "").Body.String(); !strings.Contains(list, "tax-bot") || !strings.Contains(list, "p-own") {
		t.Errorf("a global admin sees every account, got %s", list)
	}
}

// GC-6 — a scope is administered by its own agency. Reproduced 2026-10-06 as an
// admin of one agency: PATCH 200, DELETE 204, unbind 200 on another's scope.
func TestGC_ScopesAreAdministeredByTheirOwnAgency(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-tax", "runner-tax", "ag:TAX")
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('sc:tax','r-tax','runner-tax','t')`)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/scopes/sc:tax", `{"description":"mine now"}`},
		{http.MethodPut, "/api/v1/scopes/sc:tax/inventory", `{"rawInventory":"[all]\nh1\n"}`},
		{http.MethodPost, "/api/v1/scopes/sc:tax/inventory/import-hosts", `{}`},
		{http.MethodGet, "/api/v1/scopes/sc:tax/inventory", ""},
		{http.MethodPut, "/api/v1/scope-tags/sc:tax", `{"tags":["x"]}`},
		{http.MethodPut, "/api/v1/scopes/sc:tax/runners", `{"runnerIds":[]}`},
		{http.MethodPost, "/api/v1/scopes/sc:tax/runners/preview", `{"runnerIds":[]}`},
		{http.MethodDelete, "/api/v1/scopes/sc:tax", ""},
		// A scope no agency owns is shared by all of them: a global admin's.
		{http.MethodPatch, "/api/v1/scopes/sc:shared", `{"description":"x"}`},
		{http.MethodDelete, "/api/v1/scopes/sc:shared", ""},
	} {
		if rec := gateReq(t, h, c.method, c.path, gFinAdmin, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("fin admin %s %s = %d, want 403 (%s)", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scopes WHERE id IN ('sc:tax','sc:shared')`); n != 2 {
		t.Error("another agency's scope was deleted")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE scope_id='sc:tax'`); n != 1 {
		t.Error("another agency's scope binding was cleared")
	}
	// Its own scope is its own.
	if rec := gateReq(t, h, http.MethodPatch, "/api/v1/scopes/sc:fin", gFinAdmin, `{"description":"ours"}`); rec.Code != http.StatusOK {
		t.Errorf("fin admin editing its own scope = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	// Moving a scope takes authority on BOTH sides (LR-7; a global administrator
	// only until 2.3.0): FIN's administrator may neither pull TAX's scope in nor
	// push its own out — not to TAX, not to Global — on any of the setters.
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/scopes/sc:tax/agency", `{"agencyId":"ag:FIN"}`, http.StatusForbidden},
		{"/api/v1/scope-agencies", `[{"id":"sc:tax","agencyIds":["ag:FIN"]}]`, http.StatusForbidden},
		{"/api/v1/agencies/ag:FIN/members", `{"members":[{"kind":"scope","id":"sc:fin"},{"kind":"scope","id":"sc:tax"}]}`, http.StatusForbidden},
		{"/api/v1/scopes/sc:fin/agency", `{"agencyId":"ag:TAX"}`, http.StatusForbidden},
		{"/api/v1/scope-agencies", `[{"id":"sc:fin","agencyIds":["ag:TAX"]}]`, http.StatusForbidden},
		{"/api/v1/scopes/sc:fin/agency", `{"agencyId":null}`, http.StatusForbidden},
		{"/api/v1/scope-agencies", `[{"id":"sc:fin","agencyIds":["global"]}]`, http.StatusForbidden},
		// Taking a scope that is Global's is a move out of Global.
		{"/api/v1/scopes/sc:shared/agency", `{"agencyId":"ag:FIN"}`, http.StatusForbidden},
		{"/api/v1/agencies/ag:FIN/members", `{"members":[{"kind":"scope","id":"sc:fin"},{"kind":"scope","id":"sc:shared"}]}`, http.StatusForbidden},
		// Dropping its own scope from its agency would leave the scope in none.
		{"/api/v1/agencies/ag:FIN/members", `{"members":[]}`, http.StatusUnprocessableEntity},
	} {
		if rec := gateReq(t, h, http.MethodPut, c.path, gFinAdmin, c.body); rec.Code != c.want {
			t.Errorf("fin admin PUT %s %s = %d, want %d (%s)", c.path, c.body, rec.Code, c.want, rec.Body)
		}
	}
	for scope, agency := range map[string]string{"sc:tax": "ag:TAX", "sc:fin": "ag:FIN", "sc:shared": "global"} {
		if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id=? AND agency_id=?`, scope, agency); n != 1 {
			t.Errorf("%s changed agency under a departmental administrator", scope)
		}
		if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id=?`, scope); n != 1 {
			t.Errorf("%s is in %d agencies, want exactly one", scope, n)
		}
	}

	// Someone who administers both agencies moves a scope between them, and it
	// is in exactly one afterwards. A scope never has two (LR-7).
	both := gFinAdmin + "," + gTaxAdmin
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scope-agencies", both, `[{"id":"sc:fin","agencyIds":["ag:FIN","ag:TAX"]}]`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "one_agency" {
		t.Errorf("putting a scope in two agencies = %d %s, want 422 one_agency (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:fin/agency", both, `{"agencyId":"ag:TAX"}`); rec.Code != http.StatusOK {
		t.Fatalf("an administrator of both agencies moving a scope = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc:fin' AND agency_id='ag:TAX'`); n != 1 {
		t.Error("the scope did not move")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc:fin'`); n != 1 {
		t.Errorf("after the move the scope is in %d agencies, want one", n)
	}
	// They are not Global's administrator, so not into Global.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:fin/agency", both, `{"agencyId":null}`); rec.Code != http.StatusForbidden {
		t.Errorf("an administrator of two agencies moving a scope into Global = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	// The agency editor adds only what is Global's; a scope that is another
	// agency's is moved on the scope itself.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot, `{"members":[{"kind":"scope","id":"sc:fin"}]}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "one_agency" {
		t.Errorf("adding TAX's scope to FIN through the agency editor = %d %s, want 422 one_agency (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot, `{"members":[{"kind":"scope","id":"sc:shared"}]}`); rec.Code != http.StatusOK {
		t.Errorf("a global administrator adding a Global scope to FIN = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc:shared'`); n != 1 {
		t.Errorf("a scope added from Global is in %d agencies, want FIN alone", n)
	}
}

// GC-6 — a scope created by a departmental administrator is born in their
// agency. Before v2.2.2 it had none, so its creator could not see it.
func TestGC_ANewScopeLandsInItsCreatorsAgency(t *testing.T) {
	h, pool := gateServer(t)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"fin-new","hosts":["h1"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fin admin creating a scope = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies sa JOIN scopes s ON s.id=sa.scope_id
	                         WHERE s.name='fin-new' AND sa.agency_id='ag:FIN'`); n != 1 {
		t.Error("the new scope was not placed in its creator's agency")
	}
	// It may not be placed in someone else's.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"fin-sneak","agencyIds":["ag:TAX"]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin creating a scope in TAX = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scopes WHERE name='fin-sneak'`); n != 0 {
		t.Error("a refused scope was created anyway")
	}
	// A global administrator who names no agency creates it in Global — where a
	// scope "no agency owns" has belonged since migration 1220 — and nowhere else.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/scopes", gRoot, `{"scope":"everyones"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("root creating a Global scope = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies sa JOIN scopes s ON s.id=sa.scope_id
	                         WHERE s.name='everyones' AND sa.agency_id <> 'global'`); n != 0 {
		t.Error("a global administrator's scope was placed in an agency without being asked")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies sa JOIN scopes s ON s.id=sa.scope_id
	                         WHERE s.name='everyones' AND sa.agency_id = 'global'`); n != 1 {
		t.Error("a global administrator's scope is not Global's")
	}
	// An agency administrator may not create one there.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"fin-global","agencyIds":["global"]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin creating a scope in Global = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// GC-6 — replacing a runner rewrites every scope it is bound to, so it needs
// authority over each of them and not only over the replacement.
func TestGC_ReplacingARunnerNeedsEveryBoundScope(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-fin", "runner-fin", "ag:FIN")
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('sc:tax','r-gone','runner-gone','t')`)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/scope-runners/replace", gFinAdmin,
		`{"fromRunnerId":"r-gone","toRunnerId":"r-fin"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin re-pointing TAX's bound scope at its own runner = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE scope_id='sc:tax' AND runner_id='r-gone'`); n != 1 {
		t.Error("another agency's binding was rewritten")
	}
}

// GC-7, LR-69 — a host record imported for a scope follows that scope; one
// written by hand, and every bastion, belongs to an agency. Until 2.3.0 the
// hand-written kind belonged to nobody, applied to every scope, and was a
// global administrator's alone: an agency could not describe its own jump host.
func TestGC_HostAndBastionRecords(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	// What an upgraded installation holds: hand-written records and bastions
	// with no owner named, which are Global's.
	for _, r := range [][3]any{{"h-manual", "db01", nil}, {"h-fin", "fin01", "sc:fin"}, {"h-tax", "tax01", "sc:tax"}} {
		exec(`INSERT INTO ssh_hosts (id, hostname, port, source, scope_id, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?,?,22,'cronomicon',?,'seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`, r[0], r[1], r[2])
	}
	exec(`INSERT INTO bastions (id, name, hostname, address, port, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('b-old','jump-old','jump-old','10.0.0.9',22,'seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`)
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM ssh_hosts WHERE id='h-manual' AND owner_agency='global') + (SELECT COUNT(*) FROM bastions WHERE id='b-old' AND owner_agency='global')`); n != 2 {
		t.Fatal("fixture: a record with no owner named is not Global's")
	}
	const hostBody = `{"hostname":"x01","port":22}`
	const bastionBody = `{"name":"b","address":"10.0.0.1","port":22}`

	for _, c := range []struct {
		method, path, body string
		forbidden          bool
	}{
		// Global's and another agency's are not FIN's to touch.
		{http.MethodPut, "/api/v1/ssh/hosts/h-manual", hostBody, true},
		{http.MethodDelete, "/api/v1/ssh/hosts/h-manual", "", true},
		{http.MethodDelete, "/api/v1/ssh/hosts/h-manual/host-key", "", true},
		{http.MethodPost, "/api/v1/ssh/hosts/h-manual/test", "", true},
		{http.MethodPut, "/api/v1/ssh/hosts/h-tax", hostBody, true},
		{http.MethodDelete, "/api/v1/ssh/hosts/h-tax", "", true},
		{http.MethodPut, "/api/v1/ssh/bastions/b-old", bastionBody, true},
		{http.MethodDelete, "/api/v1/ssh/bastions/b-old", "", true},
		{http.MethodDelete, "/api/v1/ssh/bastions/b-old/host-key", "", true},
		{http.MethodPost, "/api/v1/ssh/bastions/b-old/test", "", true},
		// Nor may it create one in Global, or in TAX.
		{http.MethodPost, "/api/v1/ssh/hosts", `{"hostname":"x02","port":22,"ownerAgency":"global"}`, true},
		{http.MethodPost, "/api/v1/ssh/hosts", `{"hostname":"x03","port":22,"ownerAgency":"ag:TAX"}`, true},
		{http.MethodPost, "/api/v1/ssh/bastions", `{"name":"b2","address":"10.0.0.2","port":22,"ownerAgency":"global"}`, true},
		{http.MethodPost, "/api/v1/ssh/bastions", `{"name":"b3","address":"10.0.0.3","port":22,"ownerAgency":"ag:TAX"}`, true},
		// Its own scope's imported record is its own, as since 2.2.2.
		{http.MethodPut, "/api/v1/ssh/hosts/h-fin", `{"hostname":"fin01","port":2222}`, false},
	} {
		rec := gateReq(t, h, c.method, c.path, gFinAdmin, c.body)
		if got := rec.Code == http.StatusForbidden; got != c.forbidden {
			t.Errorf("fin admin %s %s = %d, want forbidden=%v (%s)", c.method, c.path, rec.Code, c.forbidden, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM ssh_hosts WHERE id IN ('h-manual','h-tax')) + (SELECT COUNT(*) FROM bastions WHERE id='b-old')`); n != 3 {
		t.Error("a record outside the caller's agency was deleted")
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM ssh_hosts WHERE hostname IN ('x02','x03')) + (SELECT COUNT(*) FROM bastions WHERE name IN ('b2','b3'))`); n != 0 {
		t.Error("a refused record was created")
	}

	// What it writes by hand is born its own: with no owner named, in its one agency.
	var made struct {
		ID          string `json:"id"`
		OwnerAgency string `json:"ownerAgency"`
	}
	rec := gateReq(t, h, http.MethodPost, "/api/v1/ssh/hosts", gFinAdmin, hostBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fin admin creating a host record = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if made.OwnerAgency != "ag:FIN" {
		t.Errorf("fin admin's host record is owned by %q, want ag:FIN", made.OwnerAgency)
	}
	finHost := made.ID
	rec = gateReq(t, h, http.MethodPost, "/api/v1/ssh/bastions", gFinAdmin, bastionBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fin admin creating a bastion = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if made.OwnerAgency != "ag:FIN" {
		t.Errorf("fin admin's bastion is owned by %q, want ag:FIN", made.OwnerAgency)
	}
	finBastion := made.ID

	// TAX's administrator cannot change either; FIN's and a global administrator can.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/ssh/hosts/" + finHost, hostBody},
		{http.MethodDelete, "/api/v1/ssh/hosts/" + finHost, ""},
		{http.MethodPut, "/api/v1/ssh/bastions/" + finBastion, bastionBody},
		{http.MethodDelete, "/api/v1/ssh/bastions/" + finBastion, ""},
	} {
		if rec := gateReq(t, h, c.method, c.path, gTaxAdmin, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("tax admin %s %s = %d, want 403 (%s)", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/"+finHost, gFinAdmin, `{"hostname":"x01","port":2200}`); rec.Code != http.StatusOK {
		t.Errorf("fin admin editing its own host record = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/bastions/"+finBastion, gRoot, bastionBody); rec.Code != http.StatusOK {
		t.Errorf("root editing FIN's bastion = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	// Giving a record to another agency is a move: authority on both sides.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/"+finHost, gFinAdmin, `{"hostname":"x01","port":22,"ownerAgency":"ag:TAX"}`); rec.Code != http.StatusForbidden {
		t.Errorf("fin admin giving its host record to TAX = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/bastions/"+finBastion, gFinAdmin, `{"name":"b","address":"10.0.0.1","port":22,"ownerAgency":"global"}`); rec.Code != http.StatusForbidden {
		t.Errorf("fin admin making its bastion Global's = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/"+finHost, gRoot, `{"hostname":"x01","port":22,"ownerAgency":"no-such-agency"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("giving a host record to an agency that does not exist = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/"+finHost, gFinAdmin+","+gTaxAdmin, `{"hostname":"x01","port":22,"ownerAgency":"ag:TAX"}`); rec.Code != http.StatusOK {
		t.Errorf("an administrator of both agencies moving a host record = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM ssh_hosts WHERE id=? AND owner_agency='ag:TAX'`, finHost); n != 1 {
		t.Error("the host record did not change owner")
	}
	// An imported record's owner is its scope's: naming one changes nothing.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/h-fin", gRoot, `{"hostname":"fin01","port":22,"ownerAgency":"ag:TAX"}`); rec.Code != http.StatusOK {
		t.Errorf("root editing an imported record = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM ssh_hosts WHERE id='h-fin' AND owner_agency='global'`); n != 1 {
		t.Error("an imported record was given an owner of its own")
	}

	// A global administrator who names no agency writes Global's, as before.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/ssh/hosts", gRoot, `{"hostname":"everyones","port":22}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if rec.Code != http.StatusCreated || made.OwnerAgency != "global" {
		t.Errorf("root creating a host record = %d owned by %q, want 201 and global (%s)", rec.Code, made.OwnerAgency, rec.Body)
	}
	// An agency that owns a record or a bastion cannot be deleted from under it.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag:OPS','OPS','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO bastions (id, name, hostname, address, port, created_at, owner_agency) VALUES ('b-ops','jump-ops','jump-ops','10.0.0.8',22,'2026-01-01T00:00:00Z','ag:OPS')`)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/ag:OPS", gRoot, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "bastion") {
		t.Errorf("deleting an agency that owns a bastion = %d (%s), want 409 naming the bastion", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM agencies WHERE id='ag:OPS'`); n != 1 {
		t.Error("the agency was deleted from under its bastion")
	}
}

// LR-72 — a hand-written host record and a bastion name only a key their owner
// may use: the owner's own, or Global's. Credential ids are listed to every
// session, so the rule is what stops a record being pointed at another
// agency's key. It binds a global administrator too.
func TestGC_ARecordNamesOnlyAKeyItsOwnerMayUse(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	for id, agency := range map[string]string{"k-tax": "ag:TAX", "k-fin": "ag:FIN", "k-shared": "global"} {
		exec(`INSERT INTO ssh_credentials (id,label,source,owner_agency,created_by,created_at,last_modified_by,last_modified_at)
		      VALUES (?,?,'stored',?,'seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`, id, id, agency)
		if agency != "global" {
			exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?,?)`, id, agency)
		}
	}
	host := func(who, cred, owner string) int {
		return gateReq(t, h, http.MethodPost, "/api/v1/ssh/hosts", who,
			`{"hostname":"h-`+cred+`-`+owner+`","port":22,"authCredentialId":"`+cred+`","ownerAgency":"`+owner+`"}`).Code
	}
	bastion := func(who, cred, owner string) int {
		return gateReq(t, h, http.MethodPost, "/api/v1/ssh/bastions", who,
			`{"name":"b-`+cred+`-`+owner+`","address":"10.0.0.1","port":22,"authCredentialId":"`+cred+`","ownerAgency":"`+owner+`"}`).Code
	}
	for _, create := range []func(who, cred, owner string) int{host, bastion} {
		for _, c := range []struct {
			who, cred, owner string
			want             int
		}{
			{gFinAdmin, "k-fin", "ag:FIN", http.StatusCreated},
			{gFinAdmin, "k-shared", "ag:FIN", http.StatusCreated},
			{gFinAdmin, "k-tax", "ag:FIN", http.StatusUnprocessableEntity},
			{gFinAdmin, "k-nope", "ag:FIN", http.StatusUnprocessableEntity},
			{gRoot, "k-tax", "ag:FIN", http.StatusUnprocessableEntity},
			{gRoot, "k-fin", "global", http.StatusUnprocessableEntity}, // a Global record names a Global key
			{gRoot, "k-shared", "global", http.StatusCreated},
		} {
			if got := create(c.who, c.cred, c.owner); got != c.want {
				t.Errorf("%s creating a record of %s with key %s = %d, want %d", c.who, c.owner, c.cred, got, c.want)
			}
		}
	}
	// On an update the rule is asked when the key or the owner changes. A record
	// from before 2.3.0 may name a key its owner could not be given today; an
	// unrelated edit of it is not refused over that (the connect is).
	exec(`INSERT INTO ssh_hosts (id, hostname, port, source, auth_credential_id, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('h-legacy','legacy01',22,'cronomicon','k-fin','seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/h-legacy", gRoot, `{"hostname":"legacy01","port":2222,"authCredentialId":"k-fin"}`); rec.Code != http.StatusOK {
		t.Errorf("editing a legacy record's port without touching its key = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/h-legacy", gRoot, `{"hostname":"legacy01","port":22,"authCredentialId":"k-tax"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("changing a Global record's key to TAX's = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	// Moving it to the agency whose key it names makes it whole.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/h-legacy", gRoot, `{"hostname":"legacy01","port":22,"authCredentialId":"k-fin","ownerAgency":"ag:FIN"}`); rec.Code != http.StatusOK {
		t.Errorf("giving the record to FIN, whose key it names = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	// And moving a record away from the agency whose key it names is refused.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/h-legacy", gRoot, `{"hostname":"legacy01","port":22,"authCredentialId":"k-fin","ownerAgency":"ag:TAX"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("giving FIN's record, with FIN's key, to TAX = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// GC-8 — a Vault path is named on the installation's one Vault connection.
func TestGC_VaultPathsAreAGlobalAdministrators(t *testing.T) {
	h, pool := gateServer(t)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"FIN_VAULT","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin binding a Vault path = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE key='FIN_VAULT'`); n != 0 {
		t.Error("a vault-source secret was written by a departmental administrator")
	}
	// A stored secret in its own agency is unaffected...
	rec = gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"FIN_STORED","source":"stored","scope":"fin-hosts","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fin admin creating a stored secret = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var sec struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)
	// ...but may not be turned into a vault-source one, by edit or by migration.
	rec = gateReq(t, h, http.MethodPut, "/api/v1/env-secrets/"+sec.ID, gFinAdmin,
		`{"key":"FIN_STORED","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin re-sourcing a secret to Vault = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+sec.ID+"/migrate-to-vault", gFinAdmin,
		`{"vaultPath":"secret/data/tax/db#password"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin migrating a secret to a Vault path = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

func jobYAML(name, scope string) string {
	y := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  run_type: bash\n  command: echo hi\n"
	if scope != "" {
		y += "  scope: " + scope + "\n"
	}
	return y
}

func publishBody(path, content string) string {
	b, _ := json.Marshal(map[string]any{"filePath": path, "content": content, "commitMessage": "m", "new": true})
	return string(b)
}

// GC-9 — publish is checked per file. The route itself only asks whether the
// caller may publish somewhere.
func TestGC_PublishIsCheckedPerFile(t *testing.T) {
	clone := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", clone)
	if err := os.MkdirAll(filepath.Join(clone, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file already in the repository that belongs to TAX.
	if err := os.WriteFile(filepath.Join(clone, "jobs", "tax-existing.yaml"), []byte(jobYAML("tax-existing", "tax-hosts")), 0o644); err != nil {
		t.Fatal(err)
	}
	h, pool := gateServer(t)
	mustExec(t, pool)(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at,source_path)
	      VALUES ('tax-nightly','git','bash','tax-hosts',1,'2026-01-01T00:00:00Z','jobs/tax-nightly.yaml')`)

	publish := func(who, path, content string) int {
		req := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/schedules/publish", who, publishBody(path, content), "If-Match", "deadbeef")
		return req.Code
	}
	for _, c := range []struct {
		name, path, content string
	}{
		{"a job in another agency's scope", "jobs/new.yaml", jobYAML("fin-new", "tax-hosts")},
		{"a job with no scope", "jobs/new.yaml", jobYAML("fin-new", "")},
		{"content whose scope cannot be read", "jobs/new.yaml", "::: not yaml :::"},
		{"a schedule file", "schedules/nightly.yaml", "apiVersion: cronomicon.io/v1\nkind: Schedule\n"},
		{"a workflow file", "workflows/wf.yaml", "apiVersion: cronomicon.io/v1\nkind: Workflow\n"},
		{"a file that already holds another agency's job", "jobs/tax-existing.yaml", jobYAML("tax-existing", "fin-hosts")},
		{"a job name another agency already uses", "jobs/other-file.yaml", jobYAML("tax-nightly", "fin-hosts")},
	} {
		if code := publish(gFinApprover, c.path, c.content); code != http.StatusForbidden {
			t.Errorf("fin approver publishing %s = %d, want 403", c.name, code)
		}
	}
	// Its own job in its own scope passes the gate. (What happens next depends on
	// a real repository, which this test does not have — anything but 403.)
	if code := publish(gFinApprover, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts")); code == http.StatusForbidden {
		t.Errorf("fin approver publishing its own job = 403, want the gate to pass")
	}
	// A global publisher is not narrowed.
	if code := publish(gRoot, "schedules/nightly.yaml", "apiVersion: cronomicon.io/v1\nkind: Schedule\n"); code == http.StatusForbidden {
		t.Errorf("root publishing a schedule file = 403, want the gate to pass")
	}
}

// seedWorkflowPair builds a workflow that reaches a TAX job only through a
// sub-workflow: parent → child → job tax-job. Returns the parent's row id.
func seedWorkflowPair(t *testing.T, pool *sql.DB) (parentRowID string) {
	t.Helper()
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('tax-job','cronomicon','bash','tax-hosts',1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, created_at)
	      VALUES ('tax-child','cronomicon','[{"type":"job","name":"tax-job"}]',1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, created_at)
	      VALUES ('parent','cronomicon','[{"type":"workflow","name":"go","workflow":"tax-child"}]',1,'2026-01-01T00:00:00Z')`)
	return rowID(t, pool, `SELECT rowid FROM workflows WHERE name='parent'`)
}

// GC-10 — a workflow is authorized on every job it will run, its sub-workflows'
// included. A parent whose only step is a sub-workflow used to authorize on
// nothing.
func TestGC_WorkflowAuthorizationFollowsSubWorkflows(t *testing.T) {
	h, pool := gateServer(t)
	parent := seedWorkflowPair(t, pool)

	// Trigger and pause as an operator of FIN: the jobs are TAX's.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows/"+parent+"/trigger", gFinOperator, `{}`); rec.Code/100 == 2 {
		t.Errorf("fin operator triggering a workflow that runs TAX's job through a sub-workflow = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPatch, "/api/v1/workflows/"+parent, gFinOperator, `{"disabled":true}`); rec.Code/100 == 2 {
		t.Errorf("fin operator pausing it = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM workflow_runs`); n != 0 {
		t.Errorf("a refused trigger started %d workflow run(s)", n)
	}

	// Compose: FIN may not author a workflow that reaches TAX's job through a child.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gFinAdmin,
		`{"name":"fin-wrapper","steps":[{"type":"workflow","name":"go","workflow":"tax-child"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin composing a workflow around TAX's sub-workflow = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM workflows WHERE name='fin-wrapper'`); n != 0 {
		t.Error("a refused workflow was saved")
	}
	// A global administrator composes it.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/workflows", gRoot,
		`{"name":"root-wrapper","steps":[{"type":"workflow","name":"go","workflow":"tax-child"}]}`)
	if rec.Code/100 != 2 {
		t.Errorf("root composing the same workflow = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
}

// GC-11 — cancelling a pending run takes a run verb on what it would touch.
func TestGC_CancellingAPendingRunNeedsARunVerb(t *testing.T) {
	h, pool := gateServer(t)
	seedWorkflowPair(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('fin-job','cronomicon','bash','fin-hosts',1,'2026-01-01T00:00:00Z')`)
	pend := func(id, kind, name, scope string) {
		var sc any
		if scope != "" {
			sc = scope
		}
		exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at)
		      VALUES (?,?,?,'cronomicon',?,'2099-01-01T00:00:00Z','someone','2026-01-01T00:00:00Z')`, id, kind, name, sc)
	}
	pend("p-wf", "workflow", "parent", "") // a pending workflow run always has an empty scope
	pend("p-fin", "job", "fin-job", "fin-hosts")

	for _, c := range []struct{ who, id, why string }{
		{gFinViewer, "p-wf", "a viewer holds no verb; the empty scope used to read as 'allowed for everyone'"},
		{gFinOperator, "p-wf", "the workflow runs TAX's job"},
		{gAllViewer, "p-fin", "reading every scope is not a run verb"},
	} {
		if rec := gateReq(t, h, http.MethodDelete, "/api/v1/pending-runs/"+c.id, c.who, ""); rec.Code == http.StatusNoContent {
			t.Errorf("%s cancelling %s = 204, want a refusal — %s", c.who, c.id, c.why)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM pending_runs`); n != 2 {
		t.Fatalf("pending runs left = %d, want both still there", n)
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/pending-runs/p-fin", gFinOperator, ""); rec.Code != http.StatusNoContent {
		t.Errorf("fin operator cancelling FIN's pending run = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/pending-runs/p-wf", gRoot, ""); rec.Code != http.StatusNoContent {
		t.Errorf("root cancelling the pending workflow run = %d, want 204 (%s)", rec.Code, rec.Body)
	}
}

// GC-12 — tags and annotations are writable by any signed-in user, on a row
// that user can read.
func TestGC_TagsAndNotesNeedAReadableRow(t *testing.T) {
	h, pool := gateServer(t)
	parent := seedWorkflowPair(t, pool)
	mustExec(t, pool)(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('fin-job','cronomicon','bash','fin-hosts',1,'2026-01-01T00:00:00Z')`)
	taxJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='tax-job'`)
	finJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='fin-job'`)

	for _, c := range []struct{ path, body string }{
		{"/api/v1/job-tags/" + taxJob, `{"tags":["pwned"]}`},
		{"/api/v1/job-annotation/" + taxJob, `{"critical":true,"contact":"x","notes":"y"}`},
		{"/api/v1/workflow-tags/" + parent, `{"tags":["pwned"]}`},
		{"/api/v1/workflow-annotation/" + parent, `{"critical":true,"contact":"x","notes":"y"}`},
	} {
		rec := gateReq(t, h, http.MethodPut, c.path, gFinViewer, c.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("fin viewer PUT %s = %d, want 404 — the row is another agency's (%s)", c.path, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "tax-") {
			t.Errorf("the refusal disclosed the row: %s", rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM jobs WHERE name='tax-job' AND tags LIKE '%pwned%'`); n != 0 {
		t.Error("another agency's job was tagged")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM annotations`); n != 0 {
		t.Error("another agency's definition was annotated")
	}
	// The rule itself is unchanged for a row the user can read: no role needed.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/job-tags/"+finJob, gFinViewer, `{"tags":["ours"]}`); rec.Code != http.StatusOK {
		t.Errorf("fin viewer tagging FIN's job = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

// GC-14 — an Apprise target's URL is its credential.
func TestGC_NotificationTargetURLsAreMasked(t *testing.T) {
	h, pool := gateServer(t)
	mustExec(t, pool)(`INSERT INTO notification_config (id, apprise_enabled, apprise_url, apprise_targets)
	      VALUES (1, 1, 'http://apprise.internal:8000',
	              '[{"label":"ops","service":"slack","url":"slack://TOKENA/TOKENB/TOKENC","enabled":true}]')
	      ON CONFLICT(id) DO UPDATE SET apprise_enabled=excluded.apprise_enabled,
	              apprise_url=excluded.apprise_url, apprise_targets=excluded.apprise_targets`)
	if body := gateReq(t, h, http.MethodGet, "/api/v1/settings/notifications", gRoot, "").Body.String(); !strings.Contains(body, "TOKENA") {
		t.Fatalf("precondition: a global admin reads the target URL back, got %s", body)
	}
	for _, who := range []string{gFinAdmin, gFinViewer, gMixed} {
		body := gateReq(t, h, http.MethodGet, "/api/v1/settings/notifications", who, "").Body.String()
		if strings.Contains(body, "TOKENA") || strings.Contains(body, "apprise.internal") {
			t.Errorf("%s read a notification target's URL: %s", who, body)
		}
		if !strings.Contains(body, `"ops"`) {
			t.Errorf("%s should still see the target's label, got %s", who, body)
		}
	}
}

// The analytics scope filter doubled its " AND " and so answered 500 to every
// caller who was not unrestricted.
func TestGC_AnalyticsAnswersARestrictedCaller(t *testing.T) {
	h, _ := gateServer(t)
	for _, who := range []string{gFinViewer, gFinAdmin, gRoot} {
		if rec := gateReq(t, h, http.MethodGet, "/api/v1/analytics/runs", who, ""); rec.Code != http.StatusOK {
			t.Errorf("%s GET /analytics/runs = %d, want 200 (%s)", who, rec.Code, rec.Body)
		}
	}
}

// GC-15 — the capability flags tell an administrator of one agency apart from
// an administrator of the installation.
func TestGC_CapabilitiesReportInstallWideAuthority(t *testing.T) {
	h, _ := gateServer(t)
	caps := func(who string) map[string]bool {
		var m map[string]bool
		rec := gateReq(t, h, http.MethodGet, "/api/v1/capabilities", who, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("capabilities for %s: %v (%s)", who, err, rec.Body)
		}
		return m
	}
	fin := caps(gFinAdmin)
	if !fin["configureApp"] || fin["configureAppGlobal"] || fin["manageRolesGlobal"] || fin["composeAdmin"] {
		t.Errorf("an admin of one agency holds configureApp somewhere and nothing install-wide, got %v", fin)
	}
	mixed := caps(gMixed)
	if mixed["configureAppGlobal"] || mixed["composeAdmin"] {
		t.Errorf("an unrestricted viewer who administers one agency is not a global admin, got %v", mixed)
	}
	root := caps(gRoot)
	for _, k := range []string{"configureAppGlobal", "manageRolesGlobal", "manageEnvVarsGlobal", "publishScheduleGlobal", "composeAdmin"} {
		if !root[k] {
			t.Errorf("a global admin must report %s", k)
		}
	}
	if composer := caps(gAllComposer); composer["composeAdmin"] || !composer["composeUnbound"] {
		t.Errorf("compose on every agency is composeUnbound and not composeAdmin, got %v", composer)
	}
}

// Review findings on the first cut of the GC band. Each of these passed the
// tests above and was still a way round the gate it sits behind.

// The store treats every source that is not exactly "stored" as Vault, so the
// gate may not look for the literal "vault".
func TestGC_VaultGateCannotBeSidesteppedBySourceSpelling(t *testing.T) {
	h, pool := gateServer(t)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"FIN_STORED","source":"stored","scope":"fin-hosts","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed stored secret = %d (%s)", rec.Code, rec.Body)
	}
	var sec struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)

	for _, c := range []struct{ name, method, path, body string }{
		{"PUT with source omitted and a vaultPath", http.MethodPut, "/api/v1/env-secrets/" + sec.ID,
			`{"key":"FIN_STORED","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`},
		{"PUT with a misspelled source", http.MethodPut, "/api/v1/env-secrets/" + sec.ID,
			`{"key":"FIN_STORED","source":"Vault","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`},
		{"POST with a misspelled source", http.MethodPost, "/api/v1/env-secrets",
			`{"key":"FIN_SNEAK","source":"VAULT","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`},
		{"POST with a stored source carrying a vaultPath", http.MethodPost, "/api/v1/env-secrets",
			`{"key":"FIN_SNEAK2","source":"stored","scope":"fin-hosts","value":"v","vaultPath":"secret/data/tax/db#password"}`},
	} {
		if rec := gateReq(t, h, c.method, c.path, gFinAdmin, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 (%s)", c.name, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE source='vault' OR vault_ref IS NOT NULL`); n != 0 {
		t.Errorf("%d secret(s) now name a Vault path", n)
	}
}

// Sync names a job with no metadata.name after the base name of its
// spec.target_host, so a nameless file could be aimed at any job's row.
func TestGC_PublishRefusesANamelessJob(t *testing.T) {
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", t.TempDir())
	h, pool := gateServer(t)
	mustExec(t, pool)(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at,source_path)
	      VALUES ('tax-nightly','git','bash','tax-hosts',1,'2026-01-01T00:00:00Z','jobs/tax-nightly.yaml')`)
	nameless := "apiVersion: cronomicon.io/v1\nkind: Job\nspec:\n  run_type: bash\n  command: echo hi\n  scope: fin-hosts\n  target_host: tax-nightly\n"
	rec := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/schedules/publish", gFinApprover,
		publishBody("jobs/x.yaml", nameless), "If-Match", "deadbeef")
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin approver publishing a nameless job aimed at TAX's job = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// A notice says a job lost its runner confinement. Dismissing it is the
// business of the scope it is about.
func TestGC_DismissingANoticeNeedsItsScope(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO retired_runner_pins (job_name, job_source, scope, runner_tag, reason, recorded_at)
	      VALUES ('tax-job','cronomicon','tax-hosts','gpu','no_scope','t'),
	             ('fin-job','cronomicon','fin-hosts','gpu','no_scope','t')`)
	taxID := rowID(t, pool, `SELECT id FROM retired_runner_pins WHERE job_name='tax-job'`)
	finID := rowID(t, pool, `SELECT id FROM retired_runner_pins WHERE job_name='fin-job'`)

	if rec := gateReq(t, h, http.MethodPost, "/api/v1/scope-binding-notices/dismiss", gFinAdmin, `{"ids":[`+taxID+`]}`); rec.Code != http.StatusForbidden {
		t.Errorf("fin admin dismissing TAX's notice = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	// One foreign id poisons the batch: nothing in it is dismissed.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/scope-binding-notices/dismiss", gFinAdmin, `{"ids":[`+finID+`,`+taxID+`]}`); rec.Code != http.StatusForbidden {
		t.Errorf("a batch naming TAX's notice = %d, want 403", rec.Code)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM retired_runner_pins WHERE dismissed_at IS NOT NULL`); n != 0 {
		t.Errorf("%d notice(s) were dismissed by a refused request", n)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/scope-binding-notices/dismiss", gFinAdmin, `{"ids":[`+finID+`]}`); rec.Code != http.StatusOK {
		t.Errorf("fin admin dismissing FIN's notice = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

// Two agencies may hold workflows of one name. A's parent must be authorized
// on the child the ENGINE will run — not on every workflow that shares the
// name, which would let B veto A's operations by creating one.
func TestGC_AnotherAgencysSameNamedWorkflowIsNotAVeto(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('fin-job','cronomicon','bash','fin-hosts',1,'2026-01-01T00:00:00Z'),
	             ('tax-job','cronomicon','bash','tax-hosts',1,'2026-01-01T00:00:00Z')`)
	// FIN's "cleanup" exists first; TAX then creates its own of the same name.
	exec(`INSERT INTO workflows (name, source, steps, enabled, created_at)
	      VALUES ('cleanup','cronomicon','[{"type":"job","name":"fin-job"}]',1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, created_at)
	      VALUES ('fin-parent','cronomicon','[{"type":"workflow","name":"go","workflow":"cleanup"}]',1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, created_at)
	      VALUES ('cleanup','cronomicon','[{"type":"job","name":"tax-job"}]',1,'2026-02-01T00:00:00Z')`)
	parent := rowID(t, pool, `SELECT rowid FROM workflows WHERE name='fin-parent'`)

	if rec := gateReq(t, h, http.MethodPatch, "/api/v1/workflows/"+parent, gFinOperator, `{"disabled":true}`); rec.Code/100 != 2 {
		t.Errorf("fin operator pausing FIN's own parent = %d, want 2xx — TAX's same-named workflow is not what it runs (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflow-tags/"+parent, gFinViewer, `{"tags":["ours"]}`); rec.Code != http.StatusOK {
		t.Errorf("fin viewer tagging FIN's own parent = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// Four cross-agency paths found after v2.2.2 shipped (2026-10-07), each
// reproduced against that release before it was closed here, in v2.2.3.
// ---------------------------------------------------------------------------

// GC-8, the keys door. A Vault-backed SSH credential names a path on the one
// Vault connection exactly as a Vault-backed secret does, and what Vault returns
// for it is delivered to a runner as key material. The gate was on the three
// secret routes only.
func TestGC_AVaultBackedSSHKeyIsAGlobalAdministrators(t *testing.T) {
	h, pool := gateServer(t)
	const vaultKey = `{"label":"not_really_a_key","source":"vault","vaultRef":"secret/data/tax/db#password"}`

	rec := gateReq(t, h, http.MethodPost, "/api/v1/ssh/credentials", gFinAdmin, vaultKey)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("fin admin creating a Vault-backed SSH key = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	// The store reads only source=="vault" as Vault; a stray vaultRef on a
	// "stored" write is refused all the same rather than argued about.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/ssh/credentials", gFinAdmin,
		`{"label":"sneaky","source":"stored","material":"x","vaultRef":"secret/data/tax/db#password"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin sending a vaultRef on a stored key = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM ssh_credentials`); n != 0 {
		t.Fatalf("a refused Vault-backed key was written (%d rows)", n)
	}

	// A stored key the agency owns cannot be re-sourced to Vault by its admin.
	mustExec(t, pool)(`INSERT INTO ssh_credentials (id,label,source,owner_agency,created_by,created_at,last_modified_by,last_modified_at)
	      VALUES ('k-fin','fin_deploy','stored','ag:FIN','seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`)
	mustExec(t, pool)(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-fin','ag:FIN')`)
	rec = gateReq(t, h, http.MethodPut, "/api/v1/ssh/credentials/k-fin", gFinAdmin,
		`{"label":"fin_deploy","source":"vault","vaultRef":"secret/data/tax/db#password"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin re-sourcing its key to Vault = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM ssh_credentials WHERE id='k-fin' AND source='stored' AND vault_ref IS NULL`); n != 1 {
		t.Error("the refused re-source changed the key")
	}
	// ...while a relabel of the same key, naming no Vault path, is still theirs.
	rec = gateReq(t, h, http.MethodPut, "/api/v1/ssh/credentials/k-fin", gFinAdmin, `{"label":"fin_deploy_2"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("fin admin relabelling its own stored key = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	// A global administrator creates one.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/ssh/credentials", gRoot, vaultKey)
	if rec.Code != http.StatusCreated {
		t.Errorf("root creating a Vault-backed SSH key = %d, want 201 (%s)", rec.Code, rec.Body)
	}
}

// A host record imported for a scope is edited by that scope's agency, and the
// write took any credential id. It now takes only a key the scope's agency may
// use: one of its own, or a shared one.
func TestGC_AHostRecordMayNotNameAnotherAgencysKey(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	key := func(id, label, agency string) {
		exec(`INSERT INTO ssh_credentials (id,label,source,owner_agency,created_by,created_at,last_modified_by,last_modified_at)
		      VALUES (?,?,'stored',?,'seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`, id, label, agency)
		if agency != "" {
			exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?,?)`, id, agency)
		}
	}
	key("k-tax", "tax_deploy", "ag:TAX")
	key("k-fin", "fin_deploy", "ag:FIN")
	key("k-shared", "shared_deploy", "")
	exec(`INSERT INTO ssh_hosts (id, hostname, port, source, scope_id, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('h-fin','fin01',22,'cronomicon','sc:fin','seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`)
	put := func(groups, cred string) *httptest.ResponseRecorder {
		return gateReq(t, h, http.MethodPut, "/api/v1/ssh/hosts/h-fin", groups, `{"hostname":"fin01","port":22,"authCredentialId":"`+cred+`"}`)
	}

	rec := put(gFinAdmin, "k-tax")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("fin admin pointing FIN's host at TAX's key = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	// The refusal for another agency's key and for a key that does not exist
	// is one and the same: it must not say which keys TAX holds.
	missing := put(gFinAdmin, "k-nope")
	if missing.Code != rec.Code || missing.Body.String() != rec.Body.String() {
		t.Errorf("another agency's key and a missing key are told apart:\n %d %s\n %d %s",
			rec.Code, rec.Body, missing.Code, missing.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM ssh_hosts WHERE id='h-fin' AND auth_credential_id IS NOT NULL`); n != 0 {
		t.Fatal("a refused write changed the host record's key")
	}
	for _, cred := range []string{"k-fin", "k-shared"} {
		if rec := put(gFinAdmin, cred); rec.Code != http.StatusOK {
			t.Errorf("fin admin naming %s = %d, want 200 (%s)", cred, rec.Code, rec.Body)
		}
	}
	// A global administrator is bound by it too since 2.3.0 (LR-72): the record
	// is FIN's scope's, and TAX's key is not FIN's to use whoever names it. An
	// id that matches nothing is a 422, not a 500.
	if rec := put(gRoot, "k-tax"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("root pointing FIN's host at TAX's key = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if rec := put(gRoot, "k-fin"); rec.Code != http.StatusOK {
		t.Errorf("root naming FIN's key for FIN's host = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := put(gRoot, "k-nope"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("root naming a missing key = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// LR-71 — a run's single target host is one of its scope's hosts: the per-run
// override, the host the job itself declares, for every caller. v2.2.3 held
// only a restricted actor's override to it; the job's own target_host was
// checked nowhere, and a global administrator was exempt.
func TestGC_ARunsTargetHostMustBeInTheScope(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:fin','fin01'), ('sc:fin','fin02')`)
	exec(`INSERT INTO jobs (name,source,run_type,scope,command,target_host,enabled,created_at) VALUES
	      ('fin-job','cronomicon','bash','fin-hosts','true','legacy-host',1,'2026-01-01T00:00:00Z'),
	      ('fin-pinned','cronomicon','bash','fin-hosts','true','fin01',1,'2026-01-01T00:00:00Z'),
	      ('platform-job','cronomicon','bash',NULL,'true','anywhere',1,'2026-01-01T00:00:00Z')`)
	id := func(name string) string { return rowID(t, pool, `SELECT rowid FROM jobs WHERE name=?`, name) }
	run := func(job, groups, body string) *httptest.ResponseRecorder {
		return gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id(job)+"/run", groups, body)
	}
	refused := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "scope_membership") {
			t.Errorf("%s = %d, want 422 scope_membership (%s)", what, rec.Code, rec.Body)
		}
	}

	refused("an override outside the scope", run("fin-pinned", gFinOperator, `{"targetHost":"tax01"}`))
	// The host the job itself declares is held to the same rule: a job in FIN's
	// scope could be authored against any host record at all.
	refused("the job's own declared host, outside its scope", run("fin-job", gFinOperator, `{}`))
	refused("the same host repeated as the override", run("fin-job", gFinOperator, `{"targetHost":"legacy-host"}`))
	// And it holds for a global administrator: the rule is the scope's, not the caller's.
	refused("a global administrator's override outside the scope", run("fin-pinned", gRoot, `{"targetHost":"tax01"}`))
	if n := count(t, pool, `SELECT COUNT(*) FROM runs`); n != 0 {
		t.Fatalf("%d refused runs were enqueued", n)
	}

	// A member of the scope is accepted: the job's own, and an override.
	if rec := run("fin-pinned", gFinOperator, `{}`); rec.Code/100 != 2 {
		t.Errorf("running a job on its own in-scope host = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
	if rec := run("fin-pinned", gFinOperator, `{"targetHost":"fin02"}`); rec.Code/100 != 2 {
		t.Errorf("an override inside the scope = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
	// An out-of-scope job is put right by overriding to a host of the scope.
	if rec := run("fin-job", gFinOperator, `{"targetHost":"fin01"}`); rec.Code/100 != 2 {
		t.Errorf("overriding an out-of-scope job to a host of its scope = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
	// A run that names a host SUBSET does not use the job's single host at all,
	// so the job's out-of-scope pin is not what it is judged on.
	if rec := run("fin-job", gFinOperator, `{"targetHosts":["fin01","fin02"]}`); rec.Code/100 != 2 {
		t.Errorf("an out-of-scope job run against a subset of its scope = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
	// A scope that lists no hosts (they live in a runner's own inventory) has no
	// membership to be outside of.
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:runner-side','runner-side','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:runner-side','ag:FIN')`)
	exec(`INSERT INTO jobs (name,source,run_type,scope,command,target_host,enabled,created_at)
	      VALUES ('fin-local','cronomicon','bash','runner-side','true','known-to-the-runner',1,'2026-01-01T00:00:00Z')`)
	if rec := run("fin-local", gFinOperator, `{}`); rec.Code == http.StatusUnprocessableEntity {
		t.Errorf("a job in a scope with no host list was refused over membership (%s)", rec.Body)
	}
	// A job with no scope has no membership to ask about: it is Global's, and
	// its target resolves against Global's host records.
	if rec := run("platform-job", gRoot, `{}`); rec.Code == http.StatusUnprocessableEntity {
		t.Errorf("a job with no scope was refused over scope membership (%s)", rec.Body)
	}

	// The composer refuses to author one, and accepts a host of the scope.
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES ('say-hi','bash','true','ssh','h1','scripts/say-hi.yaml','2026-01-01T00:00:00Z')`)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs", gFinAdmin,
		`{"name":"fin-new","scriptRef":"say-hi","scope":"fin-hosts","targetHost":"tax01"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "scope_membership") {
		t.Errorf("composing a job aimed outside its scope = %d, want 422 scope_membership (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM jobs WHERE name='fin-new'`); n != 0 {
		t.Error("a refused job was saved")
	}
	rec = gateReq(t, h, http.MethodPost, "/api/v1/jobs", gFinAdmin,
		`{"name":"fin-new","scriptRef":"say-hi","scope":"fin-hosts","targetHost":"fin02"}`)
	if rec.Code/100 != 2 {
		t.Errorf("composing a job aimed at a host of its scope = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
}

// Reference bindings need manageEnvVars on the job's own scope, from one grant.
func TestGC_ReferenceBindingsNeedThePermissionOnTheJobsScope(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	      VALUES ('g:tax-viewers','tax-viewers','viewer','ag:TAX',0,'2026-01-01T00:00:00Z')`)
	for _, j := range [][3]string{{"uid-tax", "tax-job", "tax-hosts"}, {"uid-fin", "fin-job", "fin-hosts"}} {
		exec(`INSERT INTO jobs (uid,name,source,run_type,scope,command,enabled,created_at)
		      VALUES (?,?,'cronomicon','bash',?,'true',1,'2026-01-01T00:00:00Z')`, j[0], j[1], j[2])
		exec(`INSERT INTO reference_bindings (owner_kind, owner_name, owner_source, owner_uid, ref_kind, ref_name, created_at)
		      VALUES ('job',?,'cronomicon',?,'secret','DB_PASSWORD','2026-01-01T00:00:00Z')`, j[1], j[0])
	}
	exec(`INSERT INTO jobs (uid,name,source,run_type,command,enabled,created_at)
	      VALUES ('uid-any','unscoped-job','cronomicon','bash','true',1,'2026-01-01T00:00:00Z')`)
	taxJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='tax-job'`)
	finJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='fin-job'`)
	anyJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='unscoped-job'`)
	put := func(job, groups string) int {
		return gateReq(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+job, groups, `{"bindings":[]}`).Code
	}

	// FIN's administrator, a viewer of TAX: the permission from one grant and
	// the scope from another. This answered 200 and emptied TAX's bindings.
	if code := put(taxJob, gFinAdmin+",tax-viewers"); code != http.StatusForbidden {
		t.Fatalf("FIN's admin, a viewer of TAX, rewriting TAX's job bindings = %d, want 403", code)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM reference_bindings WHERE owner_uid='uid-tax'`); n != 1 {
		t.Fatal("a refused write emptied another agency's bindings")
	}
	// The all-agencies viewer who administers FIN is the same shape (gMixed).
	if code := put(taxJob, gMixed); code != http.StatusForbidden {
		t.Errorf("gMixed rewriting TAX's job bindings = %d, want 403", code)
	}
	// A job with no scope is everyone's: a global administrator's to rebind.
	if code := put(anyJob, gMixed); code != http.StatusForbidden {
		t.Errorf("gMixed rewriting an unscoped job's bindings = %d, want 403", code)
	}
	// Their own job is theirs, and a global administrator has every job.
	if code := put(finJob, gFinAdmin+",tax-viewers"); code != http.StatusOK {
		t.Errorf("FIN's admin rewriting FIN's job bindings = %d, want 200", code)
	}
	for _, job := range []string{taxJob, anyJob} {
		if code := put(job, gRoot); code != http.StatusOK {
			t.Errorf("root rewriting job %s bindings = %d, want 200", job, code)
		}
	}
}

// GC-24 (2.2.3) — pausing or resuming a job changes it for everyone, and a job
// with no scope belongs to no one agency. The verb held on one agency is not
// authority over it.
func TestGC_PausingAJobWithNoScopeNeedsTheVerbUnbound(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('platform-job','cronomicon','bash',NULL,1,'2026-01-01T00:00:00Z'),
	             ('fin-job','cronomicon','bash','fin-hosts',1,'2026-01-01T00:00:00Z')`)
	platform := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='platform-job'`)
	fin := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='fin-job'`)
	paused := func(name string) int {
		return count(t, pool, `SELECT COUNT(*) FROM paused_jobs WHERE owner_kind='job' AND name=?`, name)
	}

	// An operator of FIN, an administrator of FIN, and an all-agencies viewer who
	// also administers FIN: none holds killJobs unbound.
	for _, who := range []string{gFinOperator, gFinAdmin, gMixed} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+platform+"/pause", who, `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s pausing a job with no scope = %d, want 403 (%s)", who, rec.Code, rec.Body)
		}
	}
	if paused("platform-job") != 0 {
		t.Fatal("a refused pause paused the job")
	}
	// A global administrator pauses it, and then nobody else may undo that.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+platform+"/pause", gRoot, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("root pausing it = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	for _, who := range []string{gFinOperator, gFinAdmin, gMixed} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+platform+"/resume", who, `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s resuming a job with no scope = %d, want 403 (%s)", who, rec.Code, rec.Body)
		}
	}
	if paused("platform-job") != 1 {
		t.Fatal("a refused resume resumed the job")
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+platform+"/resume", gRoot, `{}`); rec.Code != http.StatusOK {
		t.Errorf("root resuming it = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	// A job in an agency's own scope is that agency's to pause, as before.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+fin+"/pause", gFinOperator, `{}`); rec.Code != http.StatusOK {
		t.Errorf("fin operator pausing a FIN job = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+fin+"/resume", gFinOperator, `{}`); rec.Code != http.StatusOK {
		t.Errorf("fin operator resuming a FIN job = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}
