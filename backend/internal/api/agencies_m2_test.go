package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestRunnerAgenciesAPI exercises the M2 runner↔agency membership matrix: the CSRF
// guard, a replace-per-row assignment, membership surfaced on the runner list, the
// unknown_agency / unknown_runner 422s, and the agency_in_use 409 delete guard that
// now also fires when a runner (not just a scope) references the agency.
func TestRunnerAgenciesAPI(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, csrf := devLoginWithCSRF(t, ts)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('r1','runner-a','online','t','t')`)
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('r2','runner-b','online','t','t')`)
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a1','alpha','t')`)
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a2','beta','t')`)

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
	resp := do(http.MethodPut, "/api/v1/runner-agencies",
		[]map[string]any{{"runnerId": "r1", "agencyIds": []string{"a1"}}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Assign r1→{a1,a2}, r2→{a1} ────────────────────────────────────────────────
	resp = do(http.MethodPut, "/api/v1/runner-agencies", []map[string]any{
		{"runnerId": "r1", "agencyIds": []string{"a1", "a2"}},
		{"runnerId": "r2", "agencyIds": []string{"a1"}},
	}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT assign = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// ── Membership surfaces on the runner list ────────────────────────────────────
	resp = do(http.MethodGet, "/api/v1/runners", nil, false)
	var runners []struct {
		ID       string `json:"id"`
		Agencies []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"agencies"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&runners)
	resp.Body.Close()
	var r1Count int
	for _, r := range runners {
		if r.ID == "r1" {
			r1Count = len(r.Agencies)
		}
	}
	if r1Count != 2 {
		t.Fatalf("r1.agencies = %d, want 2", r1Count)
	}

	// ── Unknown agency / runner → 422 ─────────────────────────────────────────────
	resp = do(http.MethodPut, "/api/v1/runner-agencies",
		[]map[string]any{{"runnerId": "r1", "agencyIds": []string{"nope"}}}, true)
	code := decodeCode(t, resp)
	if code != "unknown_agency" {
		t.Fatalf("unknown agency code = %q, want unknown_agency", code)
	}
	resp = do(http.MethodPut, "/api/v1/runner-agencies",
		[]map[string]any{{"runnerId": "ghost", "agencyIds": []string{"a1"}}}, true)
	code = decodeCode(t, resp)
	if code != "unknown_runner" {
		t.Fatalf("unknown runner code = %q, want unknown_runner", code)
	}

	// ── Delete an agency a runner references → 409 agency_in_use ───────────────────
	resp = do(http.MethodDelete, "/api/v1/agencies/a1", nil, true)
	code = decodeCode(t, resp)
	if resp.StatusCode != http.StatusConflict || code != "agency_in_use" {
		t.Fatalf("delete referenced agency = %d/%q, want 409/agency_in_use", resp.StatusCode, code)
	}
}

func decodeCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	return e.Code
}
