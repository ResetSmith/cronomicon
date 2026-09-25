package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestAgenciesAPI exercises the agency catalog + scope-binding surface
// (agency-support.md M1): the CSRF guard, create + duplicate-name 409, rename,
// list, scope binding with the unknown_agency 422, the agency_in_use 409 delete
// guard while a scope references it, clearing the binding, and a clean delete.
func TestAgenciesAPI(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, csrf := devLoginWithCSRF(t, ts)

	// A scope to bind (cronomicon-source; id is opaque text).
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scopes(id, name, source, created_at) VALUES('s1','prod','cronomicon','t')`); err != nil {
		t.Fatalf("seed scope: %v", err)
	}

	do := func(method, path string, body any, withCSRF bool) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		if withCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// ── CSRF guard ──────────────────────────────────────────────────────────────
	resp := do(http.MethodPost, "/api/v1/agencies", map[string]any{"name": "alpha"}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Create ───────────────────────────────────────────────────────────────────
	resp = do(http.MethodPost, "/api/v1/agencies", map[string]any{"name": "alpha", "description": "tenant A"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == "" || created.Name != "alpha" {
		t.Fatalf("created = %+v, want non-empty id + name alpha", created)
	}

	// ── Duplicate name → 409 ──────────────────────────────────────────────────────
	resp = do(http.MethodPost, "/api/v1/agencies", map[string]any{"name": "alpha"}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", resp.StatusCode)
	}

	// ── Rename ────────────────────────────────────────────────────────────────────
	resp = do(http.MethodPut, "/api/v1/agencies/"+created.ID, map[string]any{"name": "alpha-renamed"}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// ── List contains it ──────────────────────────────────────────────────────────
	resp = do(http.MethodGet, "/api/v1/agencies", nil, false)
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0].Name != "alpha-renamed" {
		t.Fatalf("list = %+v, want one agency named alpha-renamed", list)
	}

	// ── Bind scope to unknown agency → 422 unknown_agency ─────────────────────────
	resp = do(http.MethodPut, "/api/v1/scopes/s1/agency", map[string]any{"agencyId": "does-not-exist"}, true)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bind unknown = %d, want 422", resp.StatusCode)
	}
	var ue struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ue)
	resp.Body.Close()
	if ue.Code != "unknown_agency" {
		t.Fatalf("bind unknown code = %q, want unknown_agency", ue.Code)
	}

	// ── Bind scope to the real agency → 200, surfaced on the scope ────────────────
	resp = do(http.MethodPut, "/api/v1/scopes/s1/agency", map[string]any{"agencyId": created.ID}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind = %d, want 200", resp.StatusCode)
	}
	// T3.8 — a scope carries its FULL agency SET now (scopes.agency_id was dropped in
	// migration 700). The 1:1 endpoint still exists as the Scopes tab's affordance;
	// it writes one row into scope_agencies.
	var bound struct {
		Agencies []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"agencies"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bound)
	resp.Body.Close()
	if len(bound.Agencies) != 1 || bound.Agencies[0].ID != created.ID || bound.Agencies[0].Name != "alpha-renamed" {
		t.Fatalf("scope.agencies = %+v, want exactly id %s name alpha-renamed", bound.Agencies, created.ID)
	}

	// ── Delete while referenced → 409 agency_in_use ───────────────────────────────
	resp = do(http.MethodDelete, "/api/v1/agencies/"+created.ID, nil, true)
	var inUse struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&inUse)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || inUse.Code != "agency_in_use" {
		t.Fatalf("delete in-use = %d/%q, want 409/agency_in_use", resp.StatusCode, inUse.Code)
	}

	// ── Clear the binding → 200, agency null ──────────────────────────────────────
	resp = do(http.MethodPut, "/api/v1/scopes/s1/agency", map[string]any{"agencyId": nil}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d, want 200", resp.StatusCode)
	}
	var cleared struct {
		Agency *struct{ ID string } `json:"agency"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cleared)
	resp.Body.Close()
	if cleared.Agency != nil {
		t.Fatalf("scope.agency = %+v after clear, want null", cleared.Agency)
	}

	// ── Now the delete succeeds → 204 ─────────────────────────────────────────────
	resp = do(http.MethodDelete, "/api/v1/agencies/"+created.ID, nil, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
}
