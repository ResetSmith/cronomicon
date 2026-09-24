package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestRunJobReferences — V2-11 (stored-reference additions): a manual run may
// attach existing Env Vars references (kind + bare name) on top of the job's
// declared bindings. The trigger records NAMES ONLY in the override envelope
// (JC12 — values never enter override_json); resolution stays at dispatch.
func TestRunJobReferences(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('deploy-app','bash','echo deploy','ssh','sha256:aaa','scripts/deploy-app.yaml','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name":      "ref-run",
		"scriptRef": "deploy-app",
		"scope":     "Prod",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	runURL := ts.URL + "/api/v1/jobs/" + itoa(created.ID) + "/run"

	// Additions accepted, deduped, recorded names-only in the envelope.
	resp = do(http.MethodPost, runURL, map[string]any{
		"references": []map[string]string{
			{"kind": "secret", "name": "DB_PASS"},
			{"kind": "var", "name": "REGION"},
			{"kind": "key", "name": "deploy"},
			{"kind": "var", "name": "REGION"}, // duplicate — dropped, not an error
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run with references = %d, want 202", resp.StatusCode)
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
	for _, want := range []string{
		`{"kind":"secret","name":"DB_PASS"}`,
		`{"kind":"var","name":"REGION"}`,
		`{"kind":"key","name":"deploy"}`,
	} {
		if !strings.Contains(ov.String, want) {
			t.Errorf("override_json = %q, want it to record %s", ov.String, want)
		}
	}
	if n := strings.Count(ov.String, `"REGION"`); n != 1 {
		t.Errorf("override_json = %q, want the duplicate REGION entry deduped (found %d)", ov.String, n)
	}

	// Invalid kind and invalid bare name are refused at the boundary (422), same
	// rules as a stored binding write (ReplaceBindings).
	for _, bad := range []map[string]string{
		{"kind": "bogus", "name": "X"},
		{"kind": "var", "name": "not-a-posix-name"},
	} {
		resp := do(http.MethodPost, runURL, map[string]any{"references": []map[string]string{bad}})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("run with %v = %d, want 422", bad, resp.StatusCode)
		}
		var e struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if e.Code != "invalid_binding" {
			t.Errorf("run with %v code = %q, want invalid_binding", bad, e.Code)
		}
	}

	// No references — envelope stays free of the key (older clients unchanged).
	resp = do(http.MethodPost, runURL, map[string]any{})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("plain run = %d, want 202", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	ov = sql.NullString{}
	_ = pool.QueryRow(`SELECT override_json FROM runs WHERE id=?`, run.TraceID).Scan(&ov)
	if strings.Contains(ov.String, "references") {
		t.Errorf("override_json = %q, want no references entry when the caller sends none", ov.String)
	}
}

// TestRunJobReferenceAliases — RA-4/RA-7: a per-run addition may declare `as`, the
// bare destination name its value is injected under, so a caller can feed their own
// department's row to a shared job body that reads a fixed AMADEUS_SECRET_<name>.
// The envelope records BOTH names (an entry carrying only the destination could not
// answer whose credential the run was given), and the RA-Q2 collision is refused at
// the boundary rather than as a 409 after the run is queued.
func TestRunJobReferenceAliases(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('alias-app','bash','echo deploy','ssh','sha256:bbb','scripts/alias-app.yaml','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	resp := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "alias-run", "scriptRef": "alias-app", "scope": "Prod",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	runURL := ts.URL + "/api/v1/jobs/" + itoa(created.ID) + "/run"

	// Accepted, and the envelope records name → as. The same row under TWO aliases
	// is two bindings (RA-4's dedupe key is kind+name+alias), not a duplicate.
	resp = do(http.MethodPost, runURL, map[string]any{
		"references": []map[string]string{
			{"kind": "secret", "name": "TEAMA_SUDO", "as": "BECOME_PASSWORD"},
			{"kind": "secret", "name": "TEAMA_SUDO", "as": "SUDO_PASS"},
			{"kind": "var", "name": "REGION"},
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run with aliased references = %d, want 202", resp.StatusCode)
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
	for _, want := range []string{
		`"name":"TEAMA_SUDO"`, `"as":"BECOME_PASSWORD"`, `"as":"SUDO_PASS"`,
	} {
		if !strings.Contains(ov.String, want) {
			t.Errorf("override_json = %q, want it to record %s", ov.String, want)
		}
	}
	// An un-aliased addition stays exactly as it was before Phase A — no empty `as`
	// key, so an older consumer of the envelope reads it unchanged.
	if !strings.Contains(ov.String, `{"kind":"var","name":"REGION"}`) {
		t.Errorf("override_json = %q, want the un-aliased entry unchanged", ov.String)
	}

	// A malformed alias is refused with the same rules as a row name of that kind —
	// an alias mints an env-var key exactly as a row name does.
	for _, bad := range []map[string]string{
		{"kind": "var", "name": "OK", "as": "not-a-posix-name"},
		{"kind": "var", "name": "OK", "as": "AMADEUS_FOO"},
		{"kind": "secret", "name": "OK", "as": "KEK"},
	} {
		resp := do(http.MethodPost, runURL, map[string]any{"references": []map[string]string{bad}})
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("run with alias %v = %d, want 422 (%s)", bad, resp.StatusCode, body)
		}
	}

	// RA-Q2: two DIFFERENT rows aliased to one destination. Refused — there is no
	// defensible silent winner when the values are secret material.
	resp = do(http.MethodPost, runURL, map[string]any{
		"references": []map[string]string{
			{"kind": "secret", "name": "TEAMA_SUDO", "as": "BECOME_PASSWORD"},
			{"kind": "secret", "name": "TEAMB_SUDO", "as": "BECOME_PASSWORD"},
		},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("colliding aliases = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "AMADEUS_SECRET_BECOME_PASSWORD") {
		t.Errorf("collision 422 should name the contested key: %s", body)
	}
}

// TestRunJobReferencesPermGate — attaching a reference to a run is a grant over
// stored secret/key material, so it needs ManageEnvVars (the binding-edit gate),
// not merely the ability to trigger. An operator (TriggerJobs, no ManageEnvVars)
// may still run the job plain.
func TestRunJobReferencesPermGate(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "run_refs_rbac.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// RB-15: the grant is what authorizes. These cases are about the manageEnvVars
	// PERMISSION gate, not about scoping, so both groups get an unrestricted "*"
	// grant and the scope axis stays out of the way.
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('g0','ref-operators','operator',NULL,1,'2026-01-01T00:00:00Z'),
		('g1','ref-admins','admin',NULL,1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('ref-job','git','bash','echo x','','sha256:a','jobs/ref.yaml','t')`)
	var jobID int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name='ref-job'`).Scan(&jobID); err != nil {
		t.Fatalf("rowid: %v", err)
	}

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()

	run := func(group string, body any) int {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/run", rdr)
		req.Header.Set("Remote-User", group+"@example.com")
		req.Header.Set("Remote-Groups", group)
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "tok"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	refs := map[string]any{"references": []map[string]string{{"kind": "secret", "name": "DB_PASS"}}}
	if code := run("ref-operators", refs); code != http.StatusForbidden {
		t.Errorf("operator run with references = %d, want 403 (needs ManageEnvVars)", code)
	}
	if code := run("ref-operators", nil); code != http.StatusAccepted {
		t.Errorf("operator plain run = %d, want 202 (trigger right unchanged)", code)
	}
	if code := run("ref-admins", refs); code != http.StatusAccepted {
		t.Errorf("admin run with references = %d, want 202", code)
	}
}
