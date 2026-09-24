package api_test

import (
	"net/http"
	"testing"
)

// RB-22 — PUT /agencies/{agencyId}/members, the agency-scoped member setter.
//
// The service-layer tests (settings) pin the write isolation and the RA-15 guard;
// these pin the ROUTE: the RF-1b re-homing rule restated on the inverted axis, and
// the happy path returning the resulting AgencyDetail.

// TestAgencyMembersRehomingIsAuthorized: adding an entity to an agency through the
// agency-scoped setter is the same authorization act as re-homing it through the
// entity-scoped one, and must hit the same wall. A restricted Tax admin must not
// be able to pull Finance's secret into Tax by editing TAX's member list — that
// would be the same three-request bypass RF-1b closed, reopened through the new
// endpoint.
func TestAgencyMembersRehomingIsAuthorized(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	finSecret := secretID(t, pool, "SEC_PROD")
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag-fin')`, finSecret)

	// The caller holds admin on ag:prod only. Saving ag:prod's member list with
	// Finance's secret in it is an ADD of an entity they do not own.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/agencies/ag:prod/members", "sec-admins",
		`{"members":[{"kind":"secret","id":"`+finSecret+`"}]}`); rec.Code != http.StatusForbidden {
		t.Errorf("pulling another department's secret into your own agency = %d, want 403 (%s)",
			rec.Code, rec.Body.String())
	}
	// Finance's row is untouched, and prod gained nothing.
	var fin, prod int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id=? AND agency_id='ag-fin'`, finSecret).Scan(&fin)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id=? AND agency_id='ag:prod'`, finSecret).Scan(&prod)
	if fin != 1 || prod != 0 {
		t.Errorf("refused add still moved rows (fin=%d prod=%d)", fin, prod)
	}
}

// TestAgencyMembersUnmemberedAddIsUnrestrictedOnly — RB-Q14 on the inverted axis:
// an entity in NO agency is shared infrastructure, and claiming it for a
// department requires an unrestricted actor.
func TestAgencyMembersUnmemberedAddIsUnrestrictedOnly(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	shared := secretID(t, pool, "SEC_GLOBAL") // in no agency

	if rec := reqAs(t, h, http.MethodPut, "/api/v1/agencies/ag:prod/members", "sec-admins",
		`{"members":[{"kind":"secret","id":"`+shared+`"}]}`); rec.Code != http.StatusForbidden {
		t.Errorf("restricted admin claiming an unmembered secret = %d, want 403 (RB-Q14)", rec.Code)
	}

	h2, pool2 := secretRBACServer(t, nil)
	shared2 := secretID(t, pool2, "SEC_GLOBAL")
	if _, err := pool2.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-any','Any','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	rec := reqAs(t, h2, http.MethodPut, "/api/v1/agencies/ag-any/members", "sec-admins",
		`{"members":[{"kind":"secret","id":"`+shared2+`"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unrestricted admin saving members = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// The response is the resulting AgencyDetail — the editor re-renders from it
	// without a second fetch.
	if body := rec.Body.String(); !contains2(body, `"members"`) || !contains2(body, shared2) {
		t.Errorf("response should be the resulting AgencyDetail naming the new member; got %s", body)
	}
}

// TestAgencyMembersUnknowns: an unknown agency is 404 (the path names a resource),
// an unknown entity or kind is 422 (the body is wrong), and neither writes.
func TestAgencyMembersUnknowns(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	if _, err := pool.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-x','X','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/agencies/ag-nope/members", "sec-admins",
		`{"members":[]}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown agency = %d, want 404", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/agencies/ag-x/members", "sec-admins",
		`{"members":[{"kind":"secret","id":"nope"}]}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown entity = %d, want 422", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/agencies/ag-x/members", "sec-admins",
		`{"members":[{"kind":"volcano","id":"x"}]}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown kind = %d, want 422", rec.Code)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies`).Scan(&n)
	if n != 0 {
		t.Error("a refused save wrote membership rows")
	}
}

// TestAgencyMembersGated: the route is ManageRoles-adjacent surface but gated on
// ConfigureApp like every other membership write; a viewer gets 403.
func TestAgencyMembersGated(t *testing.T) {
	h, _ := secretRBACServer(t, nil)
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/agencies/ag:prod/members", "sec-viewers",
		`{"members":[]}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer PUT members = %d, want 403", rec.Code)
	}
}

func contains2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
