package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestRbacPreflightEndpoint — the RB-4 report is ManageRoles-gated (it enumerates
// who holds what, which is the privilege-granting surface itself) and every
// findings array must serialize as [] rather than null so the SPA can map over it
// without special-casing an empty report.
func TestRbacPreflightEndpoint(t *testing.T) {
	h, _ := secretRBACServer(t, nil)

	if rec := reqAs(t, h, http.MethodGet, "/api/v1/rbac-preflight", "sec-viewers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer GET /rbac-preflight = %d, want 403 — the report names who holds what", rec.Code)
	}

	rec := reqAs(t, h, http.MethodGet, "/api/v1/rbac-preflight", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /rbac-preflight = %d (%s)", rec.Code, rec.Body.String())
	}

	var rep struct {
		UngrantedGroups         []string         `json:"ungrantedGroups"`
		EmptyMembershipEntities []map[string]any `json:"emptyMembershipEntities"`
		UnscopedJobs            []map[string]any `json:"unscopedJobs"`
		UnscopedSchedules       []map[string]any `json:"unscopedSchedules"`
		PendingUnbound          []map[string]any `json:"pendingUnbound"`
		PendingRevoked          []map[string]any `json:"pendingRevoked"`
		UsersEvaluated          int              `json:"usersEvaluated"`
		GrantsEvaluated         int              `json:"grantsEvaluated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The four conversion sections (losesExecute, multiRoleUsers, grantlessMappings,
	// wideningRestrictions, orphanScopes) were removed in v0.57.8 with the tables
	// they read. Asserting their ABSENCE matters as much as the shape of what is
	// left: a section that silently returns a permanent empty array reads to an
	// operator as an all-clear, which is the opposite of "this question can no
	// longer be asked".
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, gone := range []string{
		"losesExecute", "multiRoleUsers", "grantlessMappings",
		"wideningRestrictions", "orphanScopes", "unmappedGroups",
		"mappingsEvaluated", "restrictionsEvaluated",
	} {
		if _, present := raw[gone]; present {
			t.Errorf("%q is still in the report — it described a conversion from the "+
				"two-axis model, whose tables no longer exist", gone)
		}
	}

	for name, isNil := range map[string]bool{
		"ungrantedGroups":         rep.UngrantedGroups == nil,
		"emptyMembershipEntities": rep.EmptyMembershipEntities == nil,
		"unscopedJobs":            rep.UnscopedJobs == nil,
		"unscopedSchedules":       rep.UnscopedSchedules == nil,
		"pendingUnbound":          rep.PendingUnbound == nil,
		"pendingRevoked":          rep.PendingRevoked == nil,
	} {
		if isNil {
			t.Errorf("%s serialized as null; it must be [] so the SPA can map over it", name)
		}
	}

	// The fixture seeds grants, so the denominator proves the report ran against
	// real data rather than short-circuiting.
	if rep.GrantsEvaluated == 0 {
		t.Error("grantsEvaluated = 0 — the report must count the grants it evaluated, " +
			"so an empty findings list can be told apart from a report over no data")
	}
}

// TestCapabilitiesExposesExecuteVerbs — RB-3. The flags are FLAT UNION semantics
// ("may this actor do this somewhere") for coarse nav gating; they must be present
// and must track the caller's roles. viewDashboard/editSchedules are deliberately
// absent: RB-Q9/RB-Q13 delete them rather than advertise unenforceable permissions.
func TestCapabilitiesExposesExecuteVerbs(t *testing.T) {
	h, _ := secretRBACServer(t, nil)

	get := func(group string) map[string]bool {
		t.Helper()
		rec := reqAs(t, h, http.MethodGet, "/api/v1/capabilities", group, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET /capabilities = %d (%s)", group, rec.Code, rec.Body.String())
		}
		var caps map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return caps
	}

	admin := get("sec-admins")
	for _, k := range []string{"triggerJobs", "killJobs"} {
		if _, ok := admin[k]; !ok {
			t.Errorf("/capabilities omits %q (RB-3)", k)
		}
		if !admin[k] {
			t.Errorf("admin %s = false, want true", k)
		}
	}
	for _, k := range []string{"viewDashboard", "editSchedules"} {
		if _, ok := admin[k]; ok {
			t.Errorf("/capabilities emits %q — RB-Q9/RB-Q13 delete it; an unenforceable "+
				"permission must not be advertised as if it were real", k)
		}
	}

	viewer := get("sec-viewers")
	if viewer["triggerJobs"] || viewer["killJobs"] {
		t.Errorf("viewer reports triggerJobs=%v killJobs=%v, want false for both",
			viewer["triggerJobs"], viewer["killJobs"])
	}
	// The pre-existing flags must be untouched by the addition.
	if !admin["manageRoles"] || viewer["manageRoles"] {
		t.Error("manageRoles changed meaning when the execute verbs were added")
	}
}
