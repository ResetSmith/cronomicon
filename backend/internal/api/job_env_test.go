package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestJobComposeJobEnvRoundTrip exercises JC10 (Phase 2, the job-composer-update-2 plan):
// job-level env is persisted to jobs.env_json, echoed on the read path for an exact
// composer prefill, survives a full-state edit (the JC1 resend), and — because the
// compose PUT is a full-replace upsert — is CLEARED when an edit omits it. That last
// case documents the G0 dependency: the frontend must resend env or it is lost.
func TestJobComposeJobEnvRoundTrip(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}
	type jobResp struct {
		ID          int64             `json:"id"`
		Env         map[string]string `json:"env"`
		Description *string           `json:"description"`
	}
	dbEnv := func() sql.NullString {
		var v sql.NullString
		_ = pool.QueryRow(`SELECT env_json FROM jobs WHERE source='cronomicon' AND name='envy'`).Scan(&v)
		return v
	}

	// ── Create with job-level env ───────────────────────────────────────────────
	resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name":      "envy",
		"scriptRef": "backup-db",
		"scope":     "Prod",
		"env":       map[string]string{"STAGE": "prod", "REGION": "us"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created jobResp
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Env["STAGE"] != "prod" || created.Env["REGION"] != "us" {
		t.Fatalf("create response env = %v, want {STAGE:prod, REGION:us}", created.Env)
	}
	// Persisted to jobs.env_json.
	if e := dbEnv(); !e.Valid || !strings.Contains(e.String, `"STAGE":"prod"`) {
		t.Fatalf("jobs.env_json = %q, want it to contain STAGE:prod", e.String)
	}

	// ── Read path echoes env (GET detail → fetchJobDetail) ───────────────────────
	resp = do(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(created.ID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	var got jobResp
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Env["STAGE"] != "prod" || got.Env["REGION"] != "us" {
		t.Fatalf("GET detail env = %v, want it preserved", got.Env)
	}

	// ── Full-state edit (JC1 resend): change description, re-send env → env intact ─
	resp = do(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID), map[string]any{
		"name":        "envy",
		"scriptRef":   "backup-db",
		"scope":       "Prod",
		"description": "now with a description",
		"env":         map[string]string{"STAGE": "prod", "REGION": "us"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit = %d, want 200", resp.StatusCode)
	}
	var edited jobResp
	_ = json.NewDecoder(resp.Body).Decode(&edited)
	resp.Body.Close()
	if edited.Env["STAGE"] != "prod" || edited.Env["REGION"] != "us" {
		t.Fatalf("full-state edit dropped env: %v", edited.Env)
	}
	if edited.Description == nil || *edited.Description != "now with a description" {
		t.Fatalf("edit didn't apply the description change: %v", edited.Description)
	}

	// ── Full-replace upsert: a PUT that OMITS env clears jobs.env_json ────────────
	// This is the documented G0 dependency — without v1's full-state resend (JC1),
	// any save that omits env loses it. Phase 4's Composer must echo it back.
	resp = do(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID), map[string]any{
		"name":      "envy",
		"scriptRef": "backup-db",
		"scope":     "Prod",
	})
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusOK {
		t.Fatalf("edit-omit = %d, want 200", code)
	}
	if e := dbEnv(); e.Valid && e.String != "" && e.String != "null" {
		t.Fatalf("omitted env not cleared by the full-replace upsert: %q", e.String)
	}
}

// TestRunJobMergesJobEnv covers JC11 on the manual run path: job-level env is the
// BASE, a per-run override wins on key collision (Q-JC9), a manual run with no
// override still receives the job-level env (it applies to every run), and the
// override_json envelope records ONLY the operator's override — job-env is not
// added there, so it stays un-redacted like schedule env (JC12).
func TestRunJobMergesJobEnv(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// Job carrying job-level env {X:job, Y:job}.
	resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name":      "runny",
		"scriptRef": "backup-db",
		"scope":     "Prod",
		"env":       map[string]string{"X": "job", "Y": "job"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// runEnv triggers a manual run and returns the run's persisted env_json +
	// override_json, looked up by the trace id the trigger response echoes.
	runEnv := func(body any) (envJSON, overrideJSON string) {
		t.Helper()
		resp := do(http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(created.ID)+"/run", body)
		if resp.StatusCode != http.StatusAccepted {
			resp.Body.Close()
			t.Fatalf("run = %d, want 202", resp.StatusCode)
		}
		var run struct {
			TraceID string `json:"traceId"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&run)
		resp.Body.Close()
		var env, ov sql.NullString
		if err := pool.QueryRow(`SELECT env_json, override_json FROM runs WHERE id=?`, run.TraceID).Scan(&env, &ov); err != nil {
			t.Fatalf("fetch run %s: %v", run.TraceID, err)
		}
		return env.String, ov.String
	}

	// Override {X:run} wins X; the job-only key Y survives.
	env, ov := runEnv(map[string]any{"env": map[string]string{"X": "run"}})
	if env != `{"X":"run","Y":"job"}` {
		t.Errorf("manual run env_json = %q, want {\"X\":\"run\",\"Y\":\"job\"}", env)
	}
	// JC12: the envelope records only the operator's override, not job-env.
	if !strings.Contains(ov, `"X":"run"`) || strings.Contains(ov, `"Y":"job"`) {
		t.Errorf("override_json = %q, want only the X override recorded, not job-env Y", ov)
	}

	// A manual run with NO override still receives the job-level env (every run).
	env, _ = runEnv(map[string]any{})
	if env != `{"X":"job","Y":"job"}` {
		t.Errorf("manual run (no override) env_json = %q, want the job-level env intact", env)
	}
}
