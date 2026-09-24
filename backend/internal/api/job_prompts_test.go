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

// prompt mirrors the JobPrompt schema for decoding the compose/detail responses.
type prompt struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  *string  `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// TestJobComposePromptsRoundTrip covers UDV1 on the compose path (the sibling of the
// JC10 env round-trip): declared prompts persist to jobs.prompts_json, echo on the
// read path for an exact composer prefill, survive a full-state edit (JC1 resend),
// and — because the PUT is a full-replace upsert — are cleared to '[]' when omitted.
func TestJobComposePromptsRoundTrip(t *testing.T) {
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
	type jobResp struct {
		ID      int64    `json:"id"`
		Prompts []prompt `json:"prompts"`
	}
	dbPrompts := func() string {
		var v string
		_ = pool.QueryRow(`SELECT prompts_json FROM jobs WHERE source='amadeus' AND name='prompty'`).Scan(&v)
		return v
	}

	// ── Create with declared prompts ────────────────────────────────────────────
	resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name":      "prompty",
		"scriptRef": "backup-db",
		"scope":     "Prod",
		"prompts": []map[string]any{
			{"name": "TARGET_ENV", "label": "Deployment environment", "required": true, "options": []string{"dev", "prod"}},
			{"name": "REPLICAS", "default": "3"},
			{"name": "", "label": "dropped — empty name"}, // shape guard: nameless rows dropped
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created jobResp
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if len(created.Prompts) != 2 {
		t.Fatalf("create response prompts = %d, want 2 (empty-name dropped): %+v", len(created.Prompts), created.Prompts)
	}
	if created.Prompts[0].Name != "TARGET_ENV" || !created.Prompts[0].Required || len(created.Prompts[0].Options) != 2 {
		t.Fatalf("TARGET_ENV not round-tripped: %+v", created.Prompts[0])
	}
	if created.Prompts[1].Name != "REPLICAS" || created.Prompts[1].Default == nil || *created.Prompts[1].Default != "3" {
		t.Fatalf("REPLICAS default not round-tripped: %+v", created.Prompts[1])
	}
	if p := dbPrompts(); !strings.Contains(p, `"TARGET_ENV"`) {
		t.Fatalf("jobs.prompts_json = %q, want it to contain TARGET_ENV", p)
	}

	// ── Read path echoes prompts (GET detail → fetchJobDetail) ───────────────────
	resp = do(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(created.ID), nil)
	var got jobResp
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Prompts) != 2 || got.Prompts[0].Name != "TARGET_ENV" {
		t.Fatalf("GET detail prompts not preserved: %+v", got.Prompts)
	}

	// ── Full-state edit (JC1 resend): re-send prompts → preserved ────────────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID), map[string]any{
		"name":      "prompty",
		"scriptRef": "backup-db",
		"scope":     "Prod",
		"prompts": []map[string]any{
			{"name": "TARGET_ENV", "required": true, "options": []string{"dev", "prod"}},
			{"name": "REPLICAS", "default": "3"},
		},
	})
	var edited jobResp
	_ = json.NewDecoder(resp.Body).Decode(&edited)
	resp.Body.Close()
	if len(edited.Prompts) != 2 {
		t.Fatalf("full-state edit dropped prompts: %+v", edited.Prompts)
	}

	// ── Full-replace upsert: a PUT that OMITS prompts clears them to '[]' ─────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID), map[string]any{
		"name":      "prompty",
		"scriptRef": "backup-db",
		"scope":     "Prod",
	})
	resp.Body.Close()
	if p := dbPrompts(); p != "[]" {
		t.Fatalf("omitted prompts not cleared by the full-replace upsert: %q", p)
	}
}

// TestRunJobPromptWarnings covers UDV4/UDV6 on the manual run path: a declared
// required prompt left unfilled NEVER blocks the run (still 202), and the run's
// override_json records the unfilled name under "promptWarnings" so History can
// surface it; supplying the value (UDV2, via the env override) clears the warning.
func TestRunJobPromptWarnings(t *testing.T) {
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

	// Job with a required prompt (no default) + an optional one.
	resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name":      "prompty-run",
		"scriptRef": "backup-db",
		"scope":     "Prod",
		"prompts": []map[string]any{
			{"name": "TARGET_ENV", "required": true},
			{"name": "REPLICAS", "default": "3"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	runOverride := func(body any) string {
		t.Helper()
		resp := do(http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(created.ID)+"/run", body)
		if resp.StatusCode != http.StatusAccepted {
			resp.Body.Close()
			t.Fatalf("run = %d, want 202 (warn-only never blocks)", resp.StatusCode)
		}
		var run struct {
			TraceID string `json:"traceId"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&run)
		resp.Body.Close()
		var ov sql.NullString
		if err := pool.QueryRow(`SELECT override_json FROM runs WHERE id=?`, run.TraceID).Scan(&ov); err != nil {
			t.Fatalf("fetch run %s: %v", run.TraceID, err)
		}
		return ov.String
	}

	// Required prompt unfilled → run still accepted, warning recorded.
	if ov := runOverride(map[string]any{}); !strings.Contains(ov, `"promptWarnings":["TARGET_ENV"]`) {
		t.Errorf("override_json = %q, want promptWarnings to record the unfilled TARGET_ENV", ov)
	}

	// JR-Q1 regression — a scope Env Vars row named identically to the required prompt
	// must NOT suppress the warning. Env Vars are injected only via an explicit
	// reference binding and only as CRONOMICON_VAR_<name> (internal/runref); nothing
	// publishes a bare TARGET_ENV into the run env, so the prompt is genuinely unfilled.
	// The Run dialog used to claim "Satisfied by a scope env var." here and contradicted
	// this record; both surfaces now agree.
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('ev-target','TARGET_ENV','Prod','staging','t')`); err != nil {
		t.Fatalf("seed scope env var: %v", err)
	}
	if ov := runOverride(map[string]any{}); !strings.Contains(ov, `"promptWarnings":["TARGET_ENV"]`) {
		t.Errorf("override_json = %q, want promptWarnings STILL recorded — a scope env_vars row does not satisfy a prompt (JR-Q1)", ov)
	}

	// Required prompt supplied (as a per-run env answer, UDV2) → no warning.
	if ov := runOverride(map[string]any{"env": map[string]string{"TARGET_ENV": "prod"}}); strings.Contains(ov, "promptWarnings") {
		t.Errorf("override_json = %q, want NO promptWarnings once TARGET_ENV is supplied", ov)
	}

	// T2.4 — operator-supplied audit metadata rides alongside the server-derived
	// warnings: promptAnswers records WHERE each value came from, promptAcknowledged
	// records that the operator was warned and proceeded anyway. This is what makes a
	// run someone requested through a third party defensible after the fact.
	ov := runOverride(map[string]any{
		"promptAnswers":      map[string]string{"TARGET_ENV": "typed", "REPLICAS": "default", "NOT_DECLARED": "typed"},
		"promptAcknowledged": true,
	})
	if !strings.Contains(ov, `"promptAcknowledged":true`) {
		t.Errorf("override_json = %q, want promptAcknowledged recorded", ov)
	}
	if !strings.Contains(ov, `"TARGET_ENV":"typed"`) || !strings.Contains(ov, `"REPLICAS":"default"`) {
		t.Errorf("override_json = %q, want promptAnswers for both declared inputs", ov)
	}
	// A caller cannot stuff the envelope with names the job doesn't declare.
	if strings.Contains(ov, "NOT_DECLARED") {
		t.Errorf("override_json = %q, want undeclared promptAnswers keys dropped", ov)
	}
	// The server-derived warning still fires — an acknowledgment does not erase it.
	if !strings.Contains(ov, `"promptWarnings":["TARGET_ENV"]`) {
		t.Errorf("override_json = %q, want promptWarnings NOT suppressed by an acknowledgment", ov)
	}

	// Omitting both fields (curl, a schedule, an older client) behaves exactly as before.
	if ov := runOverride(map[string]any{}); strings.Contains(ov, "promptAnswers") || strings.Contains(ov, "promptAcknowledged") {
		t.Errorf("override_json = %q, want neither field present when the caller omits them", ov)
	}
}

// TestRunJobPromptBlockEnforcement (JR-Q5/JR-Q10, T3.5): opt-in hard enforcement.
//
//   - a `warn` job is UNCHANGED — an unfilled required input still returns 202 (UDV4
//     preserved for every job that hasn't opted in, which is all of them by default);
//   - a `block` job with an unfilled required input returns 422 prompt_required;
//   - a `block` job with the value supplied returns 202;
//   - a `block` job with promptAcknowledged returns 422 ANYWAY.
//
// That last case is the point of the whole feature and is asserted explicitly so a
// future "be helpful, let them through" change can't quietly reintroduce the escape.
// An escapable block is just `warn` with extra steps.
func TestRunJobPromptBlockEnforcement(t *testing.T) {
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
	mkJob := func(name, enforcement string) int64 {
		t.Helper()
		body := map[string]any{
			"name":      name,
			"scriptRef": "backup-db",
			"scope":     "Prod",
			"prompts":   []map[string]any{{"name": "TARGET_ENV", "required": true}},
		}
		if enforcement != "" {
			body["promptEnforcement"] = enforcement
		}
		resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", body)
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("create %s = %d, want 201", name, resp.StatusCode)
		}
		var created struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		return created.ID
	}
	run := func(id int64, body any) (int, string) {
		t.Helper()
		resp := do(http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(id)+"/run", body)
		defer resp.Body.Close()
		var e struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Code
	}

	warnJob := mkJob("warn-job", "") // omitted ⇒ warn
	blockJob := mkJob("block-job", "block")

	// The default is warn — an omitted flag must never harden a job.
	resp := do(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(warnJob), nil)
	var got struct {
		PromptEnforcement string `json:"promptEnforcement"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.PromptEnforcement != "warn" {
		t.Errorf("omitted promptEnforcement = %q, want %q", got.PromptEnforcement, "warn")
	}

	// warn: unchanged — accepted with the warning recorded (UDV4).
	if code, _ := run(warnJob, map[string]any{}); code != http.StatusAccepted {
		t.Errorf("warn job unfilled = %d, want 202 — UDV4 must hold for jobs that didn't opt in", code)
	}

	// block: unfilled → rejected.
	if code, errCode := run(blockJob, map[string]any{}); code != http.StatusUnprocessableEntity || errCode != "prompt_required" {
		t.Errorf("block job unfilled = %d/%q, want 422/prompt_required", code, errCode)
	}

	// block: supplied → accepted.
	if code, _ := run(blockJob, map[string]any{"env": map[string]string{"TARGET_ENV": "prod"}}); code != http.StatusAccepted {
		t.Errorf("block job with the value supplied = %d, want 202", code)
	}

	// block + acknowledgment → STILL rejected (JR-Q10). Do not "fix" this.
	if code, errCode := run(blockJob, map[string]any{"promptAcknowledged": true}); code != http.StatusUnprocessableEntity || errCode != "prompt_required" {
		t.Errorf("block job + promptAcknowledged = %d/%q, want 422/prompt_required — an acknowledgment must NOT escape a block job (JR-Q10)", code, errCode)
	}

	// An unrecognized mode degrades to warn rather than hardening the job.
	looseJob := mkJob("loose-job", "BLOCC")
	if code, _ := run(looseJob, map[string]any{}); code != http.StatusAccepted {
		t.Errorf("job with an unrecognized enforcement mode = %d, want 202 (normalizes to warn)", code)
	}
}
