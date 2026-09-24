package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestUpdateWorkflowTags exercises the Workflows tags write API (tags-support.md):
// the CSRF guard, a valid full-replace with normalization, the length/count/control
// caps, the missing-field 422, the unknown-workflow 404, that the tags surface on
// both the detail and list reads, and that an empty array clears them. Workflows are
// keyed by integer rowid (source-agnostic).
func TestUpdateWorkflowTags(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO workflows(name, source, description, steps, enabled, source_path, synced_at)
		 VALUES('deploy','git','nightly deploy','[]',1,'workflows/deploy.yaml','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM workflows WHERE name='deploy'`).Scan(&rowid); err != nil {
		t.Fatalf("rowid: %v", err)
	}
	id := strconv.FormatInt(rowid, 10)

	client, csrf := devLoginWithCSRF(t, ts)
	put := func(wfID string, body any, hdrCSRF bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/workflow-tags/"+wfID, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if hdrCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", wfID, err)
		}
		return resp
	}

	// ── CSRF guard ──────────────────────────────────────────────────────────────
	resp := put(id, map[string]any{"tags": []string{"prod"}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Valid full-replace + normalization ───────────────────────────────────────
	resp = put(id, map[string]any{"tags": []string{" Release ", "ops", "", "RELEASE"}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200", resp.StatusCode)
	}
	var updated struct {
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if len(updated.Tags) != 2 || updated.Tags[0] != "Release" || updated.Tags[1] != "ops" {
		t.Fatalf("normalized tags = %v, want [Release ops]", updated.Tags)
	}
	var raw string
	_ = pool.QueryRow(`SELECT tags FROM workflows WHERE name='deploy'`).Scan(&raw)
	if raw != `["Release","ops"]` {
		t.Errorf("stored tags = %q, want %q", raw, `["Release","ops"]`)
	}

	// ── Surfaces on the detail read ──────────────────────────────────────────────
	var detail struct {
		Tags []string `json:"tags"`
	}
	getJSON(t, client, ts.URL+"/api/v1/workflows/"+id, &detail)
	if len(detail.Tags) != 2 {
		t.Errorf("detail tags = %v, want 2", detail.Tags)
	}

	// ── Surfaces on the list read ────────────────────────────────────────────────
	var list struct {
		Items []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/workflows", &list)
	found := false
	for _, it := range list.Items {
		if it.Name == "deploy" {
			found = true
			if len(it.Tags) != 2 {
				t.Errorf("list tags = %v, want 2", it.Tags)
			}
		}
	}
	if !found {
		t.Error("deploy missing from list")
	}

	// ── Length boundary: 64 accepted, 65 rejected ────────────────────────────────
	resp = put(id, map[string]any{"tags": []string{strings.Repeat("y", 64)}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 64-char = %d, want 200", resp.StatusCode)
	}
	resp = put(id, map[string]any{"tags": []string{strings.Repeat("x", 65)}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT over-length = %d, want 422", resp.StatusCode)
	}

	// ── Control character → 422 ──────────────────────────────────────────────────
	resp = put(id, map[string]any{"tags": []string{"ok\ntab"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT control-char = %d, want 422", resp.StatusCode)
	}

	// ── Count cap: 30 accepted, 31 rejected ──────────────────────────────────────
	mk := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("t%d", i)
		}
		return out
	}
	resp = put(id, map[string]any{"tags": mk(30)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 30 tags = %d, want 200", resp.StatusCode)
	}
	resp = put(id, map[string]any{"tags": mk(31)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT 31 tags = %d, want 422", resp.StatusCode)
	}

	// ── Missing tags field → 422 ─────────────────────────────────────────────────
	resp = put(id, map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT without tags field = %d, want 422", resp.StatusCode)
	}

	// ── Unknown workflow → 404 ───────────────────────────────────────────────────
	resp = put("999999", map[string]any{"tags": []string{"x"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", resp.StatusCode)
	}

	// ── Clearing tags → 200 with [] ──────────────────────────────────────────────
	resp = put(id, map[string]any{"tags": []string{}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear = %d, want 200", resp.StatusCode)
	}
	_ = pool.QueryRow(`SELECT tags FROM workflows WHERE name='deploy'`).Scan(&raw)
	if raw != "[]" {
		t.Errorf("cleared tags = %q, want []", raw)
	}
}
