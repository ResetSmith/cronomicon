package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestUpdateRunnerTags exercises the runner tags write API (migration 580): the
// CSRF guard, a valid full-replace with server-side normalization, the
// missing-field 422, the unknown-runner 404, and that the tags surface on the
// GET /runners list read. Runners are keyed by their UUID id.
func TestUpdateRunnerTags(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	const rid = "019f4dea-e8e6-7587-8a2c-2d9f562f1c8d"
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO runners(id, name, status, registered_at, created_at)
		 VALUES(?, 'r1', 'online', ?, ?)`, rid, now, now); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	put := func(runnerID string, body any, hdrCSRF bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/runner-tags/"+runnerID, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if hdrCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", runnerID, err)
		}
		return resp
	}

	// ── CSRF guard ────────────────────────────────────────────────────────────
	resp := put(rid, map[string]any{"tags": []string{"prod"}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Valid full-replace + normalization (trim, drop blank, dedupe) ──────────
	resp = put(rid, map[string]any{"tags": []string{" prod ", "east", "", "PROD"}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200", resp.StatusCode)
	}
	var updated struct {
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if len(updated.Tags) != 2 || updated.Tags[0] != "prod" || updated.Tags[1] != "east" {
		t.Fatalf("normalized tags = %v, want [prod east]", updated.Tags)
	}
	var raw string
	_ = pool.QueryRow(`SELECT tags FROM runners WHERE id=?`, rid).Scan(&raw)
	if raw != `["prod","east"]` {
		t.Errorf("stored tags = %q, want %q", raw, `["prod","east"]`)
	}

	// ── Surfaces on the GET /runners list ──────────────────────────────────────
	lresp, err := client.Get(ts.URL + "/api/v1/runners")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var runners []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(lresp.Body).Decode(&runners)
	lresp.Body.Close()
	if len(runners) != 1 || len(runners[0].Tags) != 2 {
		t.Fatalf("list tags = %v, want 2 on the one runner", runners)
	}

	// ── Empty array clears ──────────────────────────────────────────────────────
	resp = put(rid, map[string]any{"tags": []string{}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT empty = %d, want 200", resp.StatusCode)
	}
	_ = pool.QueryRow(`SELECT tags FROM runners WHERE id=?`, rid).Scan(&raw)
	if raw != `[]` {
		t.Errorf("cleared tags = %q, want []", raw)
	}

	// ── Missing tags field → 422 ────────────────────────────────────────────────
	resp = put(rid, map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT missing tags = %d, want 422", resp.StatusCode)
	}

	// ── Unknown runner → 404 ────────────────────────────────────────────────────
	resp = put("no-such-runner", map[string]any{"tags": []string{"x"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", resp.StatusCode)
	}
}
