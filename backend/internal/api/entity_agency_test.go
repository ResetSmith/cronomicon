package api_test

import (
	"net/http"
	"testing"
)

// RB-16 / RB-32 — departmental ownership of the entities isolated by AGENCY rather
// than by scope. This is what makes RB-Q2's promise real: a Tax admin owns Tax's
// secrets, and cannot touch Finance's.
//
// SSH keys and runners have no scope column at all, so agency intersection is their
// only isolation (AG-Q5); secrets and variables carry membership too, and their
// routes are gated on manageEnvVars — INCLUDING reveal — so leaving that verb global
// meant anyone trusted with any department's variables could read every
// department's credential material. The plan calls that the highest-consequence gap
// in the whole program.

// TestSecretWritesAreDepartmental — a restricted manager reaches their own agency's
// secrets and nobody else's.
func TestSecretWritesAreDepartmental(t *testing.T) {
	// The admin group is restricted to `prod`, so its grant is over the agency
	// containing prod (`ag:prod`) — see secretRBACServer.
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	prodID := secretID(t, pool, "SEC_PROD")

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A second department the caller has no grant on.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)

	// Owned by the caller's agency: reachable.
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag:prod')`, prodID)
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+prodID+"/reveal", "sec-admins", ""); rec.Code != http.StatusOK {
		t.Fatalf("reveal of an own-agency secret = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Re-home it to the other department: the SAME caller, the SAME scope access,
	// now refused. Scope access is not authority over credential material.
	exec(`DELETE FROM secret_agencies WHERE secret_id = ?`, prodID)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag-fin')`, prodID)
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+prodID+"/reveal", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("reveal of ANOTHER department's secret = %d, want 403 — this is the gap "+
			"RB-32 exists to close", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-secrets/"+prodID, "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete of another department's secret = %d, want 403", rec.Code)
	}

	// An unrestricted admin reaches every department, which is what makes the
	// restriction a boundary rather than a lockout.
	h2, pool2 := secretRBACServer(t, nil)
	other := secretID(t, pool2, "SEC_PROD")
	if _, err := pool2.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-x','X','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool2.Exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag-x')`, other); err != nil {
		t.Fatal(err)
	}
	if rec := reqAs(t, h2, http.MethodPost, "/api/v1/env-secrets/"+other+"/reveal", "sec-admins", ""); rec.Code != http.StatusOK {
		t.Errorf("unrestricted admin reveal = %d, want 200 — RB-32 must narrow, not revoke", rec.Code)
	}
}

// TestUnmemberedEntityIsUnrestrictedOnly — RB-Q14, stated as a test because the
// convention is easy to read backwards.
//
// An EMPTY membership set means "no agency restriction" (AG-Q1(b)) — the entity is
// global infrastructure every department's jobs consume. It does NOT mean "member of
// nothing, therefore anyone's". Letting one department's admin rewrite or read what
// every department depends on is cross-department tampering with extra steps.
func TestUnmemberedEntityIsUnrestrictedOnly(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	globalID := secretID(t, pool, "SEC_GLOBAL") // seeded with no scope and no agency

	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+globalID+"/reveal", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("restricted reveal of an unmembered secret = %d, want 403", rec.Code)
	}

	// The same request from an unrestricted admin succeeds — the rule is about
	// reach, not about the secret being untouchable.
	h2, pool2 := secretRBACServer(t, nil)
	g2 := secretID(t, pool2, "SEC_GLOBAL")
	if rec := reqAs(t, h2, http.MethodPost, "/api/v1/env-secrets/"+g2+"/reveal", "sec-admins", ""); rec.Code != http.StatusOK {
		t.Errorf("unrestricted reveal of an unmembered secret = %d, want 200", rec.Code)
	}
}
