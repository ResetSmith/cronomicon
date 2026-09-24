package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// createStoredSecret POSTs a source='stored' secret and returns its id.
func createStoredSecret(t *testing.T, baseURL string, client *http.Client, csrf, key, value string) string {
	t.Helper()
	body := fmt.Sprintf(`{"key":%q,"source":"stored","value":%q}`, key, value)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/env-secrets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create secret = %d, want 201: %s", resp.StatusCode, b)
	}
	var sc struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return sc.ID
}

// TestRevealFailsClosedWhenAuditWriteFails is the PP-B4b regression: a reveal must
// not return the plaintext if the mandatory change_log audit write fails.
func TestRevealFailsClosedWhenAuditWriteFails(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	const secret = "supersecret-reveal-value"
	id := createStoredSecret(t, ts.URL, client, csrf, "MY_TOKEN", secret)

	// Break the audit table so WriteChangeLog returns an error.
	if _, err := pool.Exec(`DROP TABLE change_log`); err != nil {
		t.Fatalf("drop change_log: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/env-secrets/"+id+"/reveal", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("reveal with broken audit = %d, want 500; body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("PP-B4b: secret leaked in the response despite the audit write failing: %s", body)
	}
}

// TestRunOutputsRedactedAtDisplay is the PP-B4c regression: a secret value captured
// as an A12 output is masked in the run-detail API response, while the stored
// outputs_json stays raw (the engine injects it into downstream steps).
func TestRunOutputsRedactedAtDisplay(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	const secret = "topsecretoutputvalue123"
	// A stored secret supplies the redaction dictionary. (Plain env_vars values
	// no longer do — single-line Variables are log-visible, D7.)
	createStoredSecret(t, ts.URL, client, csrf, "TOK", secret)
	const trace = "run-b4c-00000001"
	if _, err := pool.Exec(
		`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, outputs_json)
		 VALUES (?, 'job1', 'bash', 'success', 't@example.com', 'manual', '2026-01-01T00:00:00Z', ?)`,
		trace, `{"TOKEN":"`+secret+`"}`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	resp, err := client.Get(ts.URL + "/api/v1/runs/" + trace)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get run = %d, want 200", resp.StatusCode)
	}
	var run struct {
		Outputs map[string]string `json:"outputs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if got := run.Outputs["TOKEN"]; got != "[REDACTED]" {
		t.Errorf("PP-B4c: output TOKEN = %q, want [REDACTED] (secret leaked at display)", got)
	}

	// Storage must remain raw so inter-job passing still works.
	var raw string
	if err := pool.QueryRow(`SELECT outputs_json FROM runs WHERE id=?`, trace).Scan(&raw); err != nil {
		t.Fatalf("read raw outputs: %v", err)
	}
	if !strings.Contains(raw, secret) {
		t.Errorf("PP-B4c: stored outputs_json was mutated (%q) — would break engine inter-job passing", raw)
	}
}
