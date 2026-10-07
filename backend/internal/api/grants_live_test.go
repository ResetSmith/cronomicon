package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// Grants are resolved per request (LR-78) and no RBAC write signs anyone out
// (LR-79). These are the route-level tests of both; auth/snapshot_test.go has
// the mechanism.

// meScopes returns the scopes GET /me reports for a member of the given groups.
func meScopes(t *testing.T, h http.Handler, groups string) []string {
	t.Helper()
	rec := gateReq(t, h, http.MethodGet, "/api/v1/me", groups, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me as %s = %d (%s)", groups, rec.Code, rec.Body)
	}
	var me struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	return me.Scopes
}

// Every route that changes what a grant reaches is in force on the caller's
// next request, for the actor and for everyone else.
//
// Each step first reads /me, so the snapshot is built and cached BEFORE the
// write: a writer that forgets auth.GrantsChanged leaves the old answer in
// place for the TTL, and its step fails. That is the standing hazard of a
// cached snapshot, and this is its test — add a step for any new writer of
// access_grants, scope_agencies, scopes or agencies.
func TestEveryGrantWriterIsInForceOnTheNextRequest(t *testing.T) {
	h, _ := gateServer(t)
	const taxOp = "tax-operators"
	want := func(step, groups string, scopes ...string) {
		t.Helper()
		if got := meScopes(t, h, groups); !slices.Equal(got, scopes) {
			t.Errorf("%s: %s sees %v, want %v", step, groups, got, scopes)
		}
	}
	do := func(step, method, path, groups, body string, code int) *httptest.ResponseRecorder {
		t.Helper()
		rec := gateReq(t, h, method, path, groups, body)
		if rec.Code != code {
			t.Fatalf("%s: %s %s as %s = %d, want %d (%s)", step, method, path, groups, rec.Code, code, rec.Body)
		}
		return rec
	}
	want("at the start", gFinAdmin, "fin-hosts")
	want("at the start", gFinViewer, "fin-hosts")
	want("at the start", taxOp)

	// An agency administrator creates a scope: theirs at once, and their
	// colleagues'. Before v2.3.0 the colleagues were signed out instead.
	rec := do("create", http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"fin-new","hosts":["h1"]}`, http.StatusCreated)
	var made struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	want("after a create", gFinAdmin, "fin-hosts", "fin-new")
	want("after a create", gFinViewer, "fin-hosts", "fin-new")

	// They rename it: the new name works and the old one is gone. This is the
	// case v2.2.2 left broken for the actor's own session.
	do("rename", http.MethodPatch, "/api/v1/scopes/"+made.ID, gFinAdmin, `{"scope":"fin-renamed","hosts":["h1"]}`, http.StatusOK)
	want("after a rename", gFinAdmin, "fin-hosts", "fin-renamed")
	want("after a rename", gFinViewer, "fin-hosts", "fin-renamed")

	// A global administrator moves a scope, by each of the three setters.
	do("move (scope route)", http.MethodPut, "/api/v1/scopes/"+made.ID+"/agency", gRoot, `{"agencyId":"ag:TAX"}`, http.StatusOK)
	want("after a move", gFinAdmin, "fin-hosts")
	want("after a move", gTaxAdmin, "fin-renamed", "tax-hosts")
	do("move (matrix)", http.MethodPut, "/api/v1/scope-agencies", gRoot, `[{"id":"`+made.ID+`","agencyIds":["ag:FIN"]}]`, http.StatusOK)
	want("after a matrix move", gFinAdmin, "fin-hosts", "fin-renamed")
	want("after a matrix move", gTaxAdmin, "tax-hosts")
	do("move (members)", http.MethodPut, "/api/v1/agencies/ag:TAX/members", gRoot,
		`{"members":[{"kind":"scope","id":"sc:tax"},{"kind":"scope","id":"`+made.ID+`"}]}`, http.StatusOK)
	want("after a members move", gTaxAdmin, "fin-renamed", "tax-hosts")

	// It is deleted.
	do("delete", http.MethodDelete, "/api/v1/scopes/"+made.ID, gRoot, "", http.StatusNoContent)
	want("after a delete", gTaxAdmin, "tax-hosts")
	want("after a delete", gFinAdmin, "fin-hosts")

	// An agency is renamed: what "My access" calls it follows (the snapshot holds
	// the names too).
	do("agency rename", http.MethodPut, "/api/v1/agencies/ag:FIN", gRoot, `{"name":"Finance"}`, http.StatusOK)
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/me/access", gFinAdmin, ""); !strings.Contains(rec.Body.String(), `"agencyName":"Finance"`) {
		t.Errorf("after an agency rename: /me/access = %s, want agencyName Finance", rec.Body)
	}

	// A grant is created, changed and removed.
	rec = do("grant", http.MethodPost, "/api/v1/access-grants", gRoot, `{"adGroup":"`+taxOp+`","role":"operator","agencyId":"ag:TAX"}`, http.StatusCreated)
	var grant struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &grant)
	want("after a grant", taxOp, "tax-hosts")
	do("grant edit", http.MethodPut, "/api/v1/access-grants/"+grant.ID, gRoot, `{"adGroup":"`+taxOp+`","role":"operator","agencyId":"ag:FIN"}`, http.StatusOK)
	want("after a grant edit", taxOp, "fin-hosts")
	do("grant delete", http.MethodDelete, "/api/v1/access-grants/"+grant.ID, gRoot, "", http.StatusNoContent)
	want("after a grant delete", taxOp)
}

// No RBAC write signs anyone out. The bystander is the dev-login cookie, the
// only cookie session an API test can mint; before v2.3.0 each of these writes
// bumped the global session epoch and the bystander's next request was a 401.
func TestNoRBACWriteSignsAnyoneOut(t *testing.T) {
	h, _ := gateServerWith(t, func(c *config.Config) { c.DevAuth = true })
	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/api/v1/auth/dev-login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("dev-login = %d, want 302", login.Code)
	}
	bystander := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		for _, c := range login.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, w := range []struct{ what, method, path, groups, body string }{
		{"a scope create", http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"fin-new","hosts":["h1"]}`},
		{"a scope rename", http.MethodPatch, "/api/v1/scopes/sc:fin", gFinAdmin, `{"scope":"fin-renamed","hosts":["h1"]}`},
		{"a scope move", http.MethodPut, "/api/v1/scope-agencies", gRoot, `[{"id":"sc:shared","agencyIds":["ag:TAX"]}]`},
		{"a grant create", http.MethodPost, "/api/v1/access-grants", gRoot, `{"adGroup":"new-group","role":"viewer","agencyId":"ag:TAX"}`},
		{"a role create", http.MethodPost, "/api/v1/roles", gRoot, `{"name":"auditor","permissions":{}}`},
	} {
		if rec := gateReq(t, h, w.method, w.path, w.groups, w.body); rec.Code/100 != 2 {
			t.Fatalf("%s = %d, want 2xx (%s)", w.what, rec.Code, rec.Body)
		}
		if code := bystander(); code != http.StatusOK {
			t.Fatalf("after %s the bystander's session = %d, want 200 — an RBAC write signed someone out (LR-79)", w.what, code)
		}
	}

	// The one way left to do it, and it is a global administrator's.
	for _, groups := range []string{gFinAdmin, gAllViewer, gMixed} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/auth/sessions/revoke", groups, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s signing everyone out = %d, want 403 (%s)", groups, rec.Code, rec.Body)
		}
	}
	if code := bystander(); code != http.StatusOK {
		t.Fatalf("a refused revocation signed the bystander out (%d)", code)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/auth/sessions/revoke", gRoot, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("root signing everyone out = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	if code := bystander(); code != http.StatusUnauthorized {
		t.Errorf("after the revocation the bystander's session = %d, want 401", code)
	}
}

// GET /me/access tells a user what their own groups grant, and which of their
// groups grant nothing (LR-87).
func TestMyAccess(t *testing.T) {
	h, _ := gateServer(t)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/me/access", gMixed+",Fin-Admins,unknown-group", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/access = %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Groups          []string `json:"groups"`
		UnmatchedGroups []string `json:"unmatchedGroups"`
		Grants          []struct {
			Role        string   `json:"role"`
			AllScopes   bool     `json:"allScopes"`
			AgencyID    string   `json:"agencyId"`
			AgencyName  string   `json:"agencyName"`
			Scopes      []string `json:"scopes"`
			Permissions []string `json:"permissions"`
			Groups      []string `json:"groups"`
			Origin      string   `json:"origin"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// A group name is matched exactly: "Fin-Admins" is not "fin-admins".
	if !slices.Equal(got.UnmatchedGroups, []string{"Fin-Admins", "unknown-group"}) {
		t.Errorf("unmatched groups = %v, want [Fin-Admins unknown-group]", got.UnmatchedGroups)
	}
	if len(got.Grants) != 2 {
		t.Fatalf("grants = %+v, want the FIN admin grant and the all-agencies viewer grant", got.Grants)
	}
	for _, g := range got.Grants {
		switch g.Role {
		case "admin":
			if g.AllScopes || g.AgencyID != "ag:FIN" || g.AgencyName != "FIN" || !slices.Equal(g.Scopes, []string{"fin-hosts"}) ||
				!slices.Equal(g.Groups, []string{gFinAdmin}) || g.Origin != "group" || !slices.Contains(g.Permissions, "configureApp") {
				t.Errorf("the FIN admin grant = %+v", g)
			}
		case "viewer":
			if !g.AllScopes || g.AgencyID != "" || len(g.Permissions) != 0 || !slices.Equal(g.Groups, []string{gAllViewer}) || g.Origin != "group" {
				t.Errorf("the all-agencies viewer grant = %+v", g)
			}
		default:
			t.Errorf("unexpected grant %+v", g)
		}
	}

	// It is about the caller only, and needs a session.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/access", nil)
	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, req)
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("GET /me/access with no session = %d, want 401", anon.Code)
	}
}
