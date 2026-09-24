package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

type apiBinding struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Reference string `json:"reference"`
}

// TestReferenceBindingsAPI exercises the reference-binding surface
// (vault-integration.md P1.1): the CSRF/permission gate, a valid full-replace on
// a script and a job, the derived-reference echo, validation 422s, the unknown-
// owner 404, and the body scan (suggested + bare-name lint).
func TestReferenceBindingsAPI(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	// Seed a script (catalog row) whose body references two Env Vars, one via the
	// derived form and one bare, plus an Env Vars secret row named DB_PASS so the
	// bare-name lint has something known to flag.
	body := "#!/bin/bash\necho \"$CRONOMICON_SECRET_DB_PASS\"\ncurl -H \"t: $REGION\"\n"
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scripts(name, run_type, script, content_hash, synced_at) VALUES('deploy','bash',?,'sha256:x',?)`,
		body, now); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO secrets(id, key, source, created_by, created_at, last_modified_by, last_modified_at)
		 VALUES('s1','DB_PASS','stored','t',?,'t',?)`, now, now); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO env_vars(id, key, value, created_by, created_at, last_modified_by, last_modified_at)
		 VALUES('v1','REGION','us-east','t',?,'t',?)`, now, now); err != nil {
		t.Fatalf("seed var: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)

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

	const scriptPath = "/api/v1/script-reference-bindings/deploy"

	// ── CSRF guard on write ─────────────────────────────────────────────────────
	resp := do(http.MethodPut, scriptPath, map[string]any{"bindings": []any{}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Valid full-replace (dedupe applied, derived reference echoed) ────────────
	resp = do(http.MethodPut, scriptPath, map[string]any{"bindings": []map[string]string{
		{"kind": "secret", "name": "DB_PASS"},
		{"kind": "var", "name": "REGION"},
		{"kind": "secret", "name": "DB_PASS"}, // dup
	}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Bindings []apiBinding `json:"bindings"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Bindings) != 2 {
		t.Fatalf("want 2 bindings after dedupe, got %+v", out.Bindings)
	}
	// Sorted (kind, name): secret DB_PASS then var REGION.
	if out.Bindings[0].Reference != "CRONOMICON_SECRET_DB_PASS" || out.Bindings[1].Reference != "CRONOMICON_VAR_REGION" {
		t.Fatalf("derived references wrong: %+v", out.Bindings)
	}

	// ── GET reflects the stored set ─────────────────────────────────────────────
	resp = do(http.MethodGet, scriptPath, nil, false)
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Bindings) != 2 {
		t.Fatalf("GET want 2, got %+v", out.Bindings)
	}

	// ── Validation: reserved KEK secret name → 422 ──────────────────────────────
	resp = do(http.MethodPut, scriptPath, map[string]any{"bindings": []map[string]string{
		{"kind": "secret", "name": "KEK"},
	}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT reserved KEK = %d, want 422", resp.StatusCode)
	}
	// The rejected write must not have replaced the prior set.
	resp = do(http.MethodGet, scriptPath, nil, false)
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Bindings) != 2 {
		t.Fatalf("rejected PUT changed stored set: %+v", out.Bindings)
	}

	// ── Unknown script → 404 ────────────────────────────────────────────────────
	resp = do(http.MethodGet, "/api/v1/script-reference-bindings/nope", nil, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown script = %d, want 404", resp.StatusCode)
	}

	// ── Scan: suggested (derived) + bare-name lint ──────────────────────────────
	resp = do(http.MethodGet, "/api/v1/script-reference-scan/deploy", nil, false)
	var scan struct {
		Suggested      []apiBinding `json:"suggested"`
		BareReferences []apiBinding `json:"bareReferences"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&scan)
	resp.Body.Close()
	// Body names CRONOMICON_SECRET_DB_PASS (derived) → suggested; bare $REGION matches
	// the known var row → bareReferences.
	if len(scan.Suggested) != 1 || scan.Suggested[0].Reference != "CRONOMICON_SECRET_DB_PASS" {
		t.Fatalf("scan suggested wrong: %+v", scan.Suggested)
	}
	if len(scan.BareReferences) != 1 || scan.BareReferences[0].Name != "REGION" {
		t.Fatalf("scan bareReferences wrong: %+v", scan.BareReferences)
	}

	// ── Job bindings: seed an amadeus job, replace + read back by rowid ──────────
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs(name, source, run_type, command, content_hash, synced_at)
		 VALUES('j1','amadeus','bash','echo hi','sha256:y',?)`, now); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	var jobID int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name='j1' AND source='amadeus'`).Scan(&jobID); err != nil {
		t.Fatalf("job rowid: %v", err)
	}
	jobPath := "/api/v1/job-reference-bindings/" + strconv.FormatInt(jobID, 10)
	resp = do(http.MethodPut, jobPath, map[string]any{"bindings": []map[string]string{
		{"kind": "key", "name": "deploy_key"},
	}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT job binding = %d, want 200", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Bindings) != 1 || out.Bindings[0].Reference != "CRONOMICON_KEY_deploy_key" {
		t.Fatalf("job binding wrong: %+v", out.Bindings)
	}
}

// TestRunReferencesAPI (P1.8): GET /runs/{traceId}/references returns the references
// a run ACTUALLY injected, read from the P1.6 dispatch-injection audit (change_log)
// — names + derived reference only. A run with an audit row returns its references;
// a run with no audit returns empty; an unknown run is 404.
func TestRunReferencesAPI(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	mkRun := func(id, scope string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx,
			`INSERT INTO runs(id, job_name, job_source, run_type, scope, status, executor, triggered_by, trigger_kind, created_at)
			 VALUES(?, 'deployjob', 'git', 'bash', ?, 'success', 'runner', 'ops', 'manual', ?)`, id, scope, now); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	// A dispatched run with an injection audit (secret + var); the audit is already
	// the deduped, key-free set the resolver injected.
	mkRun("trace-refs-1", "prod")
	details := `{"scope":"prod","references":[{"kind":"secret","name":"DB_PASS","source":"stored"},{"kind":"var","name":"REGION","source":"stored"}]}`
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO change_log(at, actor, category, action, target, details, created_at)
		 VALUES(?, 'ops', 'Secrets', 'injected', 'trace-refs-1', ?, ?)`, now, details, now); err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	// A run that injected nothing (no audit row).
	mkRun("trace-refs-none", "prod")

	client, _ := devLoginWithCSRF(t, ts)
	get := func(id string) (int, []apiBinding) {
		t.Helper()
		resp, err := client.Get(ts.URL + "/api/v1/runs/" + id + "/references")
		if err != nil {
			t.Fatalf("GET references %s: %v", id, err)
		}
		var out struct {
			References []apiBinding `json:"references"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		code := resp.StatusCode
		resp.Body.Close()
		return code, out.References
	}

	code, refs := get("trace-refs-1")
	if code != http.StatusOK {
		t.Fatalf("GET references = %d, want 200", code)
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 injected references, got %+v", refs)
	}
	seen := map[string]bool{}
	for _, b := range refs {
		seen[b.Reference] = true
	}
	if !seen["CRONOMICON_SECRET_DB_PASS"] || !seen["CRONOMICON_VAR_REGION"] {
		t.Fatalf("references wrong: %+v", refs)
	}

	// A run with no injection audit → empty (200).
	if code, refs := get("trace-refs-none"); code != http.StatusOK || len(refs) != 0 {
		t.Fatalf("no-audit run = %d / %+v, want 200 / empty", code, refs)
	}

	// Unknown run → 404.
	resp2, _ := client.Get(ts.URL + "/api/v1/runs/nope/references")
	code2 := resp2.StatusCode
	resp2.Body.Close()
	if code2 != http.StatusNotFound {
		t.Errorf("GET references for unknown run = %d, want 404", code2)
	}
}
