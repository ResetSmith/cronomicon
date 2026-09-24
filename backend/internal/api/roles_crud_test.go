package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

type roleRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Builtin     bool   `json:"builtin"`
	Rank        int    `json:"rank"`
	Permissions struct {
		TriggerJobs     bool `json:"triggerJobs"`
		KillJobs        bool `json:"killJobs"`
		ManageEnvVars   bool `json:"manageEnvVars"`
		PublishSchedule bool `json:"publishSchedule"`
		ConfigureApp    bool `json:"configureApp"`
		ManageRoles     bool `json:"manageRoles"`
	} `json:"permissions"`
}

func listRoles(t *testing.T, h http.Handler) []roleRow {
	t.Helper()
	rec := reqAs(t, h, http.MethodGet, "/api/v1/roles", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /roles = %d (%s)", rec.Code, rec.Body.String())
	}
	var out []roleRow
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode roles: %v", err)
	}
	return out
}

func findRole(rs []roleRow, name string) (roleRow, bool) {
	for _, r := range rs {
		if r.Name == name {
			return r, true
		}
	}
	return roleRow{}, false
}

// TestRolesAreDataAndSeeded — RB-6/RB-7. The four built-ins come from migration
// 800 now, not from a Go literal, and they must carry EXACTLY the matrix that was
// compiled in (minus the two deleted permissions).
func TestRolesAreDataAndSeeded(t *testing.T) {
	h, _ := secretRBACServer(t, nil)
	roles := listRoles(t, h)
	if len(roles) != 4 {
		t.Fatalf("got %d roles, want the 4 built-ins: %+v", len(roles), roles)
	}
	// Ordered by descending rank, then name.
	if roles[0].Name != "admin" {
		t.Errorf("roles[0] = %q, want admin (highest rank first)", roles[0].Name)
	}
	for _, r := range roles {
		if !r.Builtin {
			t.Errorf("role %q is not marked builtin", r.Name)
		}
	}
	admin, _ := findRole(roles, "admin")
	if !admin.Permissions.ManageRoles || !admin.Permissions.ConfigureApp || admin.Rank != 3 {
		t.Errorf("admin = %+v, want every permission and rank 3", admin)
	}
	approver, _ := findRole(roles, "approver")
	if !approver.Permissions.PublishSchedule || approver.Permissions.ConfigureApp {
		t.Errorf("approver = %+v, want publishSchedule and NOT configureApp", approver.Permissions)
	}
	// approver's rank is seeded deliberately: the old hardcoded precedence map
	// omitted it, so an approver-only user rendered a blank role.
	if approver.Rank == 0 {
		t.Error("approver rank is 0 — the migration must not reproduce the old rolePrecedence omission")
	}
	viewer, _ := findRole(roles, "viewer")
	if viewer.Permissions != (struct {
		TriggerJobs     bool `json:"triggerJobs"`
		KillJobs        bool `json:"killJobs"`
		ManageEnvVars   bool `json:"manageEnvVars"`
		PublishSchedule bool `json:"publishSchedule"`
		ConfigureApp    bool `json:"configureApp"`
		ManageRoles     bool `json:"manageRoles"`
	}{}) {
		t.Errorf("viewer = %+v, want NO permissions — its only one was viewDashboard, which v0.56.1 deletes",
			viewer.Permissions)
	}
}

// TestRoleCRUDLifecycle — RB-8. A custom role can be created, used in a mapping,
// edited, and deleted; and the delete is refused while the mapping exists.
func TestRoleCRUDLifecycle(t *testing.T) {
	h, _ := secretRBACServer(t, nil)

	// Create. The name is canonicalized, so "Tax-Operator" lands as "tax-operator".
	body := `{"name":"Tax-Operator","description":"Tax dept runner","rank":2,
	          "permissions":{"triggerJobs":true,"killJobs":true}}`
	rec := reqAs(t, h, http.MethodPost, "/api/v1/roles", "sec-admins", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /roles = %d (%s)", rec.Code, rec.Body.String())
	}
	var created roleRow
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Name != "tax-operator" {
		t.Errorf("created name = %q, want the canonicalized tax-operator", created.Name)
	}
	if created.Builtin {
		t.Error("a created role must not be builtin — that flag is what makes a role undeletable")
	}

	// It is now a valid role everywhere, which is the entire point of RB-6.
	if _, ok := findRole(listRoles(t, h), "tax-operator"); !ok {
		t.Fatal("custom role missing from GET /roles")
	}

	// A duplicate (in any casing) is a 409.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/roles", "sec-admins", body); rec.Code != http.StatusConflict {
		t.Errorf("duplicate POST = %d, want 409", rec.Code)
	}

	// Edit: grant it manageEnvVars.
	put := `{"name":"tax-operator","description":"Tax dept runner","rank":2,
	         "permissions":{"triggerJobs":true,"killJobs":true,"manageEnvVars":true}}`
	rec = reqAs(t, h, http.MethodPut, "/api/v1/roles/tax-operator", "sec-admins", put)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /roles/tax-operator = %d (%s)", rec.Code, rec.Body.String())
	}
	after, _ := findRole(listRoles(t, h), "tax-operator")
	if !after.Permissions.ManageEnvVars {
		t.Error("edit did not take effect in the registry — RefreshRoles may not be wired to the write path")
	}

	// RF-7: a GRANT referencing the role blocks the delete too. The guard counted
	// only ad_group_mappings, with a comment promising "Phase 2 adds access_grants
	// to this check" — Phase 2 never did, and since access_grants.role has no
	// foreign key, deleting the role left grants pointing at a role that no longer
	// exists: they resolve to zero permissions, i.e. silent revocation.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/access-grants", "sec-admins",
		`{"adGroup":"sg-tax","role":"tax-operator","allScopes":true}`); rec.Code != http.StatusCreated {
		t.Fatalf("POST /access-grants with a custom role = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/roles/tax-operator", "sec-admins", ""); rec.Code != http.StatusConflict {
		t.Errorf("DELETE of a GRANTED role = %d, want 409 role_in_use (RF-7)", rec.Code)
	}
	// Remove the grant and the delete goes through.
	grec := reqAs(t, h, http.MethodGet, "/api/v1/access-grants", "sec-admins", "")
	var grants []struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &grants)
	for _, g := range grants {
		if g.Role == "tax-operator" {
			if rec := reqAs(t, h, http.MethodDelete, "/api/v1/access-grants/"+g.ID, "sec-admins", ""); rec.Code >= 300 {
				t.Fatalf("could not remove grant: %d", rec.Code)
			}
		}
	}

	// The only live reference is gone, so the delete goes through. The legacy
	// ad_group_mappings check that used to guard this was removed in v0.57.7 (it
	// had become an unclearable 409 once its UI went) and its table in v0.57.8;
	// access_grants is now the sole referential guard, which is what the block
	// above proves.
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/roles/tax-operator", "sec-admins", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /roles/tax-operator = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := findRole(listRoles(t, h), "tax-operator"); ok {
		t.Error("deleted role still present")
	}
}

// TestRoleLockoutFloors — RB-8. The instance must never be able to lock itself out
// of its own access screen. These are the guards with no recovery path if wrong.
func TestRoleLockoutFloors(t *testing.T) {
	h, _ := secretRBACServer(t, nil)

	// admin may not give up manageRoles, at all, ever.
	strip := `{"name":"admin","rank":3,"permissions":{"triggerJobs":true,"killJobs":true,
	           "manageEnvVars":true,"publishSchedule":true,"configureApp":true,"manageRoles":false}}`
	rec := reqAs(t, h, http.MethodPut, "/api/v1/roles/admin", "sec-admins", strip)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stripping manageRoles from admin = %d, want 409 — nothing could restore it", rec.Code)
	}
	admin, _ := findRole(listRoles(t, h), "admin")
	if !admin.Permissions.ManageRoles {
		t.Fatal("admin lost manageRoles despite the 409 — the write must not have been attempted")
	}

	// Built-ins cannot be deleted.
	for _, name := range []string{"admin", "viewer"} {
		if rec := reqAs(t, h, http.MethodDelete, "/api/v1/roles/"+name, "sec-admins", ""); rec.Code != http.StatusConflict {
			t.Errorf("DELETE built-in %q = %d, want 409", name, rec.Code)
		}
	}

	// A built-in's other permissions ARE editable — an operator wanting Approver
	// without publishSchedule should not have to invent a parallel role.
	edit := `{"name":"approver","rank":2,"permissions":{"triggerJobs":true,"killJobs":true,"publishSchedule":false}}`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/roles/approver", "sec-admins", edit); rec.Code != http.StatusOK {
		t.Fatalf("editing a built-in = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	approver, _ := findRole(listRoles(t, h), "approver")
	if approver.Permissions.PublishSchedule {
		t.Error("built-in edit did not persist")
	}
	if !approver.Builtin {
		t.Error("editing a built-in cleared its builtin flag — that would make it deletable")
	}
}

// TestRolesGateAndUnknownRole — the surface stays ManageRoles-gated, and an
// unknown role is a 404 rather than a 500.
func TestRolesGateAndUnknownRole(t *testing.T) {
	h, _ := secretRBACServer(t, nil)

	body := `{"name":"nope","permissions":{}}`
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/roles", body},
		{http.MethodPut, "/api/v1/roles/viewer", body},
		{http.MethodDelete, "/api/v1/roles/viewer", ""},
	} {
		if rec := reqAs(t, h, tc.method, tc.path, "sec-viewers", tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/roles/ghost", "sec-admins", body); rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown role = %d, want 404", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/roles/ghost", "sec-admins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown role = %d, want 404", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/roles", "sec-admins", `{"name":"  ","permissions":{}}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST blank name = %d, want 422", rec.Code)
	}
}
