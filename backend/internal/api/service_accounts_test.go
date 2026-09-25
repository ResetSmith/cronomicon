package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ET-A/ET-B — service accounts and the machine trigger surface.
//
// The property under test throughout is that a token is a PRINCIPAL, not a
// bypass: it authenticates without a session, and then meets every gate a click
// meets. The tests that matter most are therefore the refusals — an
// unauthenticated call, a revoked token, a job that never opted in — because
// each of them is a way the surface could quietly become an unauthenticated
// remote-execution endpoint.

func mintAccount(t *testing.T, ts *httptest.Server, client *http.Client, csrf string, body map[string]any) (string, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/service-accounts", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		t.Fatalf("mint status = %d, want 201: %s", resp.StatusCode, buf.String())
	}
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if out.Token == "" {
		t.Fatal("mint returned no token")
	}
	return out.ID, out.Token
}

func seedTriggerJob(t *testing.T, pool *sql.DB, name string, requestable int) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO jobs (name, source, run_type, concurrency_policy, enabled, requestable, synced_at)
		VALUES (?, 'git', 'bash', 'Allow', 1, ?, '2026-08-11T00:00:00Z')`, name, requestable); err != nil {
		t.Fatalf("seed job %s: %v", name, err)
	}
}

func triggerAs(t *testing.T, ts *httptest.Server, token, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	return resp
}

// The happy path, and the provenance it must leave behind.
func TestServiceTokenTriggersRequestableJob(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "nightly-export", 1)

	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "nagios", "role": "admin", "allScopes": true,
	})

	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/nightly-export")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		t.Fatalf("trigger status = %d, want 202: %s", resp.StatusCode, buf.String())
	}

	// The run must record the service account as its actor and 'webhook' as its
	// provenance — a token trigger that looked like a manual one would make the
	// audit trail lie about who started the work.
	var actor, kind string
	if err := pool.QueryRow(
		`SELECT triggered_by, trigger_kind FROM runs WHERE job_name = 'nightly-export'`).Scan(&actor, &kind); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if actor != "svc:nagios" {
		t.Errorf("triggered_by = %q, want svc:nagios", actor)
	}
	if kind != "webhook" {
		t.Errorf("trigger_kind = %q, want webhook", kind)
	}

	// last_used_at answers "can I revoke this?" and must be stamped.
	var lastUsed sql.NullString
	_ = pool.QueryRow(`SELECT last_used_at FROM service_accounts WHERE name='nagios'`).Scan(&lastUsed)
	if !lastUsed.Valid {
		t.Error("last_used_at was not stamped on a successful authentication")
	}
}

// PF-Q15 — the opt-in. requestable defaults to 0, so a job nobody opted in is
// closed even to a fully unrestricted token.
func TestServiceTokenRefusedOnNonRequestableJob(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "internal-only", 0)

	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "ci", "role": "admin", "allScopes": true,
	})

	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/internal-only")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an unrestricted token must still respect the per-job opt-in", resp.StatusCode)
	}
	var body struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != "not_requestable" {
		t.Errorf("error code = %q, want not_requestable", body.Code)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='internal-only'`).Scan(&n)
	if n != 0 {
		t.Errorf("a refused trigger still created %d runs", n)
	}
}

// The surface must be unreachable without a credential. If this ever passes a
// request through, the endpoint is unauthenticated remote execution.
func TestTriggerRefusesMissingAndBadTokens(t *testing.T) {
	ts, pool := newTestServer(t)
	seedTriggerJob(t, pool, "open-job", 1)

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"garbage token", "crnsvc_not-a-real-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := triggerAs(t, ts, tc.token, "/api/v1/trigger/jobs/open-job")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	if n != 0 {
		t.Errorf("unauthenticated calls created %d runs", n)
	}
}

// Revocation must take effect immediately — there is no session to expire.
func TestRevokedServiceTokenStopsWorking(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "open-job", 1)

	id, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "doomed", "role": "admin", "allScopes": true,
	})

	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/open-job")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pre-revoke status = %d, want 202", resp.StatusCode)
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/service-accounts/"+id, nil)
	req.Header.Set("X-CSRF-Token", csrf)
	dresp, err := client.Do(req)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", dresp.StatusCode)
	}

	resp2 := triggerAs(t, ts, token, "/api/v1/trigger/jobs/open-job")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("post-revoke status = %d, want 401", resp2.StatusCode)
	}
}

// A name in both sources must not be resolved by guessing.
func TestTriggerRefusesAmbiguousNameAndAcceptsSourceParam(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "shared-name", 1)
	if _, err := pool.Exec(`
		INSERT INTO jobs (name, source, run_type, concurrency_policy, enabled, requestable, synced_at)
		VALUES ('shared-name', 'cronomicon', 'bash', 'Allow', 1, 1, '2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("seed cronomicon twin: %v", err)
	}

	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "ci", "role": "admin", "allScopes": true,
	})

	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/shared-name")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("ambiguous name status = %d, want 409", resp.StatusCode)
	}
	var body struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != "ambiguous_name" {
		t.Errorf("error code = %q, want ambiguous_name", body.Code)
	}

	resp2 := triggerAs(t, ts, token, "/api/v1/trigger/jobs/shared-name?source=cronomicon")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("?source=cronomicon status = %d, want 202", resp2.StatusCode)
	}
	var src string
	if err := pool.QueryRow(`SELECT job_source FROM runs WHERE job_name='shared-name'`).Scan(&src); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if src != "cronomicon" {
		t.Errorf("job_source = %q, want cronomicon — ?source= picked the wrong definition", src)
	}
}

// A token carries a role and a WHERE, and both must bind. An account scoped to
// an agency holding no scopes is zero access, not "all" — the A5 tri-state.
func TestServiceTokenScopeIsEnforced(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-1','Tax','2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	// A job in a scope the agency does NOT contain.
	if _, err := pool.Exec(`
		INSERT INTO jobs (name, source, run_type, scope, concurrency_policy, enabled, requestable, synced_at)
		VALUES ('finance-close', 'git', 'bash', 'finance', 'Allow', 1, 1, '2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "tax-bot", "role": "admin", "agencyId": "ag-1",
	})

	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/finance-close")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 — an empty agency grant is zero access, never unrestricted", resp.StatusCode)
	}
}

// Minting is a role grant, so it must be gated and validated like one.
func TestServiceAccountCreateValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	post := func(body map[string]any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/service-accounts", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"no shape", map[string]any{"name": "a", "role": "admin"}, http.StatusBadRequest},
		{"both shapes", map[string]any{"name": "b", "role": "admin", "allScopes": true, "agencyId": "ag-x"}, http.StatusBadRequest},
		{"unknown role", map[string]any{"name": "c", "role": "wizard", "allScopes": true}, http.StatusBadRequest},
		{"unknown agency", map[string]any{"name": "d", "role": "admin", "agencyId": "nope"}, http.StatusBadRequest},
		{"colon in name", map[string]any{"name": "has:colon", "role": "admin", "allScopes": true}, http.StatusBadRequest},
		{"past expiry", map[string]any{"name": "e", "role": "admin", "allScopes": true, "expiresAt": "2000-01-01T00:00:00Z"}, http.StatusBadRequest},
		{"valid", map[string]any{"name": "f", "role": "admin", "allScopes": true}, http.StatusCreated},
		{"duplicate name", map[string]any{"name": "f", "role": "admin", "allScopes": true}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := post(tc.body); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// The list must never leak token material, in any field.
func TestServiceAccountListNeverCarriesTokenMaterial(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "listed", "role": "admin", "allScopes": true,
	})

	resp, err := client.Get(ts.URL + "/api/v1/service-accounts")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	body := buf.String()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d: %s", resp.StatusCode, body)
	}
	for _, needle := range []string{token, "token_hash", "tokenHash"} {
		if bytes.Contains([]byte(body), []byte(needle)) {
			t.Errorf("list response contains %q — the plaintext is shown once and the hash never leaves the DB", needle)
		}
	}
	if !bytes.Contains([]byte(body), []byte(`"listed"`)) {
		t.Errorf("list did not include the account: %s", body)
	}
	fmt.Fprint(&buf, "") // keep buf used on all paths
}
