package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Phase D (RA-9/RA-10, the runas-update plan) at the API boundary.

type shadowRow struct {
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	Scope        string   `json:"scope"`
	Agencies     []string `json:"agencies"`
	Unrestricted bool     `json:"unrestricted"`
	Reason       string   `json:"reason"`
}

func getShadows(t *testing.T, h http.Handler, as string) []shadowRow {
	t.Helper()
	rec := reqAs(t, h, http.MethodGet, "/api/v1/env-var-shadows", as, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /env-var-shadows as %s = %d (%s)", as, rec.Code, rec.Body.String())
	}
	var body struct {
		Shadows []shadowRow `json:"shadows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Shadows == nil {
		t.Fatal("shadows must serialize as [] rather than null — a client rendering " +
			"\"no warnings\" should not have to special-case a nil")
	}
	return body.Shadows
}

// TestEnvVarShadowsAreScopeFiltered — a finding names a row AND a scope, so an
// unfiltered list would tell a restricted operator which keys exist in scopes their
// own Env Vars list hides (P1.7/M3). The warning is only useful to the person who
// can act on it, and it must not become a cross-scope existence oracle for anyone
// else.
func TestEnvVarShadowsAreScopeFiltered(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"viewer": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const now = "2026-01-01T00:00:00Z"
	// Two shadows: one in prod, one in staging. Each scoped row sits on a global twin.
	exec(`INSERT INTO secrets(id,key,source,created_at) VALUES('s-g','TOKEN','stored',?)`, now)
	exec(`INSERT INTO secrets(id,key,scope,source,created_at) VALUES('s-p','TOKEN','prod','stored',?)`, now)
	exec(`INSERT INTO env_vars(id,key,value,created_at) VALUES('v-g','REGION','us',?)`, now)
	exec(`INSERT INTO env_vars(id,key,value,scope,created_at) VALUES('v-s','REGION','eu','staging',?)`, now)

	// The unrestricted admin sees both.
	if got := getShadows(t, h, "sec-admins"); len(got) != 2 {
		t.Fatalf("unrestricted admin shadows = %+v, want 2", got)
	}
	// The prod-restricted viewer sees only the prod one.
	got := getShadows(t, h, "sec-viewers")
	if len(got) != 1 {
		t.Fatalf("prod-restricted viewer shadows = %+v, want 1 (prod only)", got)
	}
	if got[0].Scope != "prod" {
		t.Errorf("restricted viewer saw scope %q — staging must be filtered out", got[0].Scope)
	}
	if !got[0].Unrestricted {
		t.Error("an unmembered scoped row must be flagged: it wins for EVERY department's runs in that scope")
	}
	if got[0].Reason == "" {
		t.Error("a warning with no reason is not actionable")
	}
}

// TestEnvVarShadowsRequireASession — the endpoint names rows, so it sits behind the
// same session gate as the lists it decorates. Built by hand rather than through
// reqAs, which always supplies an identity header: the point here is the request
// that carries NONE.
func TestEnvVarShadowsRequireASession(t *testing.T) {
	h, _ := secretRBACServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/env-var-shadows", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("unauthenticated GET /env-var-shadows = 200, want an auth failure (%s)", rec.Body.String())
	}
}

// TestCreationAgencyAmbiguityIsRefused — RA-9 inherits only where "their
// department" has ONE answer. An actor holding the verb on several agencies must
// still name one: silently enrolling the row in all of them would widen it past
// what they are likely to have meant, and picking one for them would be a guess
// about ownership.
func TestCreationAgencyAmbiguityIsRefused(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A SECOND departmental admin grant for the same group — now "their agency" has
	// two answers. (secretRBACServer already granted sec-admins admin over ag:prod.)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-second','Second','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc-second','prod2','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-second','ag-second')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at)
	      VALUES ('g-second','sec-admins','admin','ag-second',0,'2026-01-01T00:00:00Z')`)

	rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"AMBIGUOUS","value":"x","scope":"prod"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with two candidate departments = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	var e struct{ Code, Message string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "agency_required" {
		t.Errorf("code = %q, want agency_required", e.Code)
	}
	// The message must name the candidates — the actor holds the permission on all
	// of them, so this reveals nothing they cannot already list, and without it the
	// 422 is a dead end.
	for _, want := range []string{"ag:prod", "ag-second"} {
		if !contains(e.Message, want) {
			t.Errorf("422 message should name candidate %q: %s", want, e.Message)
		}
	}
	// Naming one explicitly still works.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"AMBIGUOUS","value":"x","scope":"prod","agencyIds":["ag:prod"]}`); rec.Code != http.StatusCreated {
		t.Errorf("explicit agency after an ambiguous refusal = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
}
