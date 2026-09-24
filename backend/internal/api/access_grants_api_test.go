package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

type grantRow struct {
	ID         string `json:"id"`
	AdGroup    string `json:"adGroup"`
	Role       string `json:"role"`
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	AllScopes  bool   `json:"allScopes"`
}

func listGrants(t *testing.T, h http.Handler) []grantRow {
	t.Helper()
	rec := reqAs(t, h, http.MethodGet, "/api/v1/access-grants", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /access-grants = %d (%s)", rec.Code, rec.Body.String())
	}
	var out []grantRow
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestAccessGrantsCRUD — RB-18. The grant surface is ManageRoles-gated and
// enforces the one-shape-per-grant rule (RB-Q1) at the boundary, so a caller gets
// an explanatory 422 rather than a 500 from the CHECK constraint.
func TestAccessGrantsCRUD(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	if _, err := pool.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('a-tax','Tax','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}

	// The fixture seeds the grants that back its own identities (RB-15 — they are
	// authoritative now, so the fixture cannot omit them). Count from that baseline
	// rather than from zero.
	baseline := len(listGrants(t, h))

	// Agency-shaped.
	rec := reqAs(t, h, http.MethodPost, "/api/v1/access-grants", "sec-admins",
		`{"adGroup":"sg-tax","role":"operator","agencyId":"a-tax"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST agency grant = %d (%s)", rec.Code, rec.Body.String())
	}
	var created grantRow
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// "*"-shaped.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/access-grants", "sec-admins",
		`{"adGroup":"sg-admins","role":"admin","allScopes":true}`); rec.Code != http.StatusCreated {
		t.Fatalf("POST all-scopes grant = %d (%s)", rec.Code, rec.Body.String())
	}

	grants := listGrants(t, h)
	if len(grants) != baseline+2 {
		t.Fatalf("got %d grants, want %d: %+v", len(grants), baseline+2, grants)
	}
	// The agency name is denormalized so a grants table renders a chip without a
	// second fetch.
	for _, g := range grants {
		if g.AgencyID == "a-tax" && g.AgencyName != "Tax" {
			t.Errorf("agencyName = %q, want the denormalized Tax", g.AgencyName)
		}
	}

	// Shape rule: exactly one of agency / allScopes.
	for _, bad := range []struct{ name, body string }{
		{"neither", `{"adGroup":"sg-x","role":"operator"}`},
		{"both", `{"adGroup":"sg-x","role":"operator","agencyId":"a-tax","allScopes":true}`},
		{"unknown agency", `{"adGroup":"sg-x","role":"operator","agencyId":"nope"}`},
		{"unknown role", `{"adGroup":"sg-x","role":"wizard","agencyId":"a-tax"}`},
		{"blank group", `{"adGroup":"  ","role":"operator","agencyId":"a-tax"}`},
	} {
		if rec := reqAs(t, h, http.MethodPost, "/api/v1/access-grants", "sec-admins", bad.body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST %s = %d, want 422", bad.name, rec.Code)
		}
	}

	// Duplicates collide on the dedupe index and report as a conflict, not a 500.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/access-grants", "sec-admins",
		`{"adGroup":"sg-tax","role":"operator","agencyId":"a-tax"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate grant = %d, want 409", rec.Code)
	}

	// Update, then delete.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/access-grants/"+created.ID, "sec-admins",
		`{"adGroup":"sg-tax-2","role":"viewer","agencyId":"a-tax"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/access-grants/"+created.ID, "sec-admins", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/access-grants/"+created.ID, "sec-admins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of a gone grant = %d, want 404", rec.Code)
	}
}

// TestAccessGrantsGated — the grant surface IS the privilege-granting surface, so
// reads and writes alike are ManageRoles-only.
func TestAccessGrantsGated(t *testing.T) {
	h, _ := secretRBACServer(t, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/access-grants", ""},
		{http.MethodPost, "/api/v1/access-grants", `{"adGroup":"x","role":"viewer","allScopes":true}`},
		{http.MethodPut, "/api/v1/access-grants/abc", `{"adGroup":"x","role":"viewer","allScopes":true}`},
		{http.MethodDelete, "/api/v1/access-grants/abc", ""},
	} {
		if rec := reqAs(t, h, tc.method, tc.path, "sec-viewers", tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

// TestAgencyDeleteRefusedWhileGranted — RB-17. access_grants.agency_id is ON
// DELETE CASCADE, so deleting an agency would silently revoke every grant authored
// against it. Refuse instead: quiet revocation is the failure this surface exists
// to prevent.
func TestAgencyDeleteRefusedWhileGranted(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	if _, err := pool.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('a-fin','Finance','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/access-grants", "sec-admins",
		`{"adGroup":"sg-fin","role":"operator","agencyId":"a-fin"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed grant = %d (%s)", rec.Code, rec.Body.String())
	}

	before := len(listGrants(t, h))
	rec := reqAs(t, h, http.MethodDelete, "/api/v1/agencies/a-fin", "sec-admins", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE granted agency = %d, want 409 — cascading would silently revoke the grant", rec.Code)
	}
	if len(listGrants(t, h)) != before {
		t.Error("the grant was removed despite the refusal")
	}
}
