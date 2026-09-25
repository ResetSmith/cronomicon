package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The Phase-2 HTTP surface (the agencies plan T2.6/T2.12) and — more
// importantly — the claim that Phase 2 changes NO behavior. That claim is what
// makes the phase reversible and what lets the pre-flight report be read against
// production data before Phase 3, so it is asserted rather than assumed.

type membershipRow struct {
	ID        string   `json:"id"`
	AgencyIDs []string `json:"agencyIds"`
}

// TestAgencyMembershipEndpointsGate — reads are session-gated so views can render
// membership; writes need ConfigureApp + CSRF, the same gate as the agency
// catalog, runner membership and the scope binding. A weaker gate on the secret
// side would let a secrets manager re-home an isolation zone an admin owns.
func TestAgencyMembershipEndpointsGate(t *testing.T) {
	h, _ := secretRBACServer(t, nil)
	for _, kind := range []string{"scope", "secret", "env-var", "ssh-credential"} {
		path := "/api/v1/" + kind + "-agencies"
		if rec := reqAs(t, h, http.MethodGet, path, "sec-viewers", ""); rec.Code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200 (reads are session-gated)", path, rec.Code)
		}
		// sec-viewers maps to the viewer role, which lacks ConfigureApp.
		if rec := reqAs(t, h, http.MethodPut, path, "sec-viewers", `[]`); rec.Code != http.StatusForbidden {
			t.Errorf("viewer PUT %s = %d, want 403 (writes need ConfigureApp)", path, rec.Code)
		}
		if rec := reqAs(t, h, http.MethodPut, path, "sec-admins", `[]`); rec.Code != http.StatusOK {
			t.Errorf("admin PUT %s = %d, want 200", path, rec.Code)
		}
	}
}

// TestAgencyMembershipRoundTripAPI — assign, read back, and confirm an unknown id
// is refused 422 without a partial write.
func TestAgencyMembershipRoundTripAPI(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	const now = "2026-01-01T00:00:00Z"
	if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-dss','DSS',?)`, now); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	var secretID string
	if err := pool.QueryRow(`SELECT id FROM secrets WHERE key='SEC_PROD'`).Scan(&secretID); err != nil {
		t.Fatalf("secret id: %v", err)
	}

	body := `[{"id":"` + secretID + `","agencyIds":["ag-dss"]}]`
	rec := reqAs(t, h, http.MethodPut, "/api/v1/secret-agencies", "sec-admins", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	var got []membershipRow
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != secretID || len(got[0].AgencyIDs) != 1 {
		t.Fatalf("membership = %+v", got)
	}

	bad := `[{"id":"nope","agencyIds":["ag-dss"]}]`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/secret-agencies", "sec-admins", bad); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("PUT with an unknown secret = %d, want 422", rec.Code)
	}
}

// TestAgencyPreflightEndpoint — the T2.12 report is ConfigureApp-gated and reports
// the AG-Q1(b) and AG-Q5 halves separately so each can be accepted on its own.
func TestAgencyPreflightEndpoint(t *testing.T) {
	h, _ := secretRBACServer(t, nil)
	if rec := reqAs(t, h, http.MethodGet, "/api/v1/agency-preflight", "sec-viewers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer GET /agency-preflight = %d, want 403", rec.Code)
	}
	rec := reqAs(t, h, http.MethodGet, "/api/v1/agency-preflight", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /agency-preflight = %d (%s)", rec.Code, rec.Body.String())
	}
	var rep struct {
		ReferenceFindings         []map[string]any `json:"referenceFindings"`
		KeyFindings               []map[string]any `json:"keyFindings"`
		JobBindingsChecked        int              `json:"jobBindingsChecked"`
		ScriptBindingsUnevaluated int              `json:"scriptBindingsUnevaluated"`
		MembershipAssigned        int              `json:"membershipAssigned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Both arrays must serialize as [] rather than null — a client rendering "no
	// findings" should not have to special-case a nil.
	if rep.ReferenceFindings == nil || rep.KeyFindings == nil {
		t.Errorf("findings serialized as null, want []: %s", rec.Body.String())
	}
}

// TestPhase3AppliesTheAgencyPredicate is the inverse of the Phase-2 test it
// replaces. Through v0.52.2 the membership tables were populated but INERT, and
// TestPhase2ChangesNoResolution asserted exactly that — assigning membership that
// would break a reference under Phase 3 had to change nothing. Phase 3 flips the
// predicate, so that test correctly began failing and is now this one: the same
// setup, the opposite expectation.
//
// Keeping it as a live assertion (rather than deleting it) is the point — it is the
// only test that would notice if the agency clause were quietly dropped from
// resolution while the membership UI kept pretending it meant something.
func TestPhase3AppliesTheAgencyPredicate(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	const now = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-dss','DSS',?)`, now)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-prod','prod','cronomicon',?)`, now)
	var secretID string
	if err := pool.QueryRow(`SELECT id FROM secrets WHERE key='SEC_GLOBAL'`).Scan(&secretID); err != nil {
		t.Fatalf("secret id: %v", err)
	}

	validate := func() bool {
		t.Helper()
		rec := reqAs(t, h, http.MethodPost, "/api/v1/references/validate", "sec-admins",
			`{"scope":"prod","references":[{"kind":"secret","name":"SEC_GLOBAL"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("validate = %d (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Results []struct {
				OK bool `json:"ok"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Results) != 1 {
			t.Fatalf("want 1 verdict, got %d", len(out.Results))
		}
		return out.Results[0].OK
	}

	// A secret with NO membership is unrestricted — the state migration 670 leaves
	// every existing row in, and what keeps the tightening from being an upgrade cliff.
	if !validate() {
		t.Fatal("precondition: an unrestricted global secret must resolve from prod")
	}

	// Put the secret in DSS while the prod scope belongs to no agency: the sets no
	// longer intersect, so under AG-Q1(b) it stops resolving.
	body := `[{"id":"` + secretID + `","agencyIds":["ag-dss"]}]`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/secret-agencies", "sec-admins", body); rec.Code != http.StatusOK {
		t.Fatalf("assign membership = %d (%s)", rec.Code, rec.Body.String())
	}
	if validate() {
		t.Fatal("agency membership did not narrow resolution — the AG-Q1(b) clause is not being applied")
	}

	// Putting the SCOPE in the same agency restores it: intersection, not exclusion.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/scope-agencies", "sec-admins",
		`[{"id":"sc-prod","agencyIds":["ag-dss"]}]`); rec.Code != http.StatusOK {
		t.Fatalf("assign scope membership = %d (%s)", rec.Code, rec.Body.String())
	}
	if !validate() {
		t.Fatal("a scope sharing the secret's agency must resolve it — the clause is an intersection, not a denylist")
	}
}
