package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestUpdateScriptTags exercises the Scripts tags write API (scripts-tags.md):
// the CSRF guard, a valid full-replace with normalization (trim / dedupe /
// blank-drop), the over-length 422, the unknown-script 404, that the new tags
// surface on both the detail and list reads, and that an empty array clears them.
func TestUpdateScriptTags(t *testing.T) {
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

	put := func(name string, body any, hdrCSRF bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/script-tags/"+name, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if hdrCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", name, err)
		}
		return resp
	}

	// ── CSRF guard ──────────────────────────────────────────────────────────────
	resp := put("backup-db", map[string]any{"tags": []string{"prod"}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Valid full-replace + normalization ───────────────────────────────────────
	// " Prod " trims, "" drops, "PROD" collapses into "Prod" (first casing wins).
	resp = put("backup-db", map[string]any{"tags": []string{" Prod ", "db", "", "PROD"}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200", resp.StatusCode)
	}
	var updated struct {
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if len(updated.Tags) != 2 || updated.Tags[0] != "Prod" || updated.Tags[1] != "db" {
		t.Fatalf("normalized tags = %v, want [Prod db]", updated.Tags)
	}

	// Persisted to the DB as a JSON array.
	var raw string
	_ = pool.QueryRow(`SELECT tags FROM scripts WHERE name='backup-db'`).Scan(&raw)
	if raw != `["Prod","db"]` {
		t.Errorf("stored tags = %q, want %q", raw, `["Prod","db"]`)
	}

	// ── Surfaces on the detail read ──────────────────────────────────────────────
	var detail struct {
		Tags []string `json:"tags"`
	}
	getJSON(t, client, ts.URL+"/api/v1/scripts/backup-db", &detail)
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
	getJSON(t, client, ts.URL+"/api/v1/scripts", &list)
	found := false
	for _, it := range list.Items {
		if it.Name == "backup-db" {
			found = true
			if len(it.Tags) != 2 {
				t.Errorf("list tags = %v, want 2", it.Tags)
			}
		}
	}
	if !found {
		t.Error("backup-db missing from list")
	}

	// ── Length boundary: 64 chars accepted, 65 rejected (off-by-one guard) ────────
	resp = put("backup-db", map[string]any{"tags": []string{strings.Repeat("y", 64)}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 64-char (the max) = %d, want 200", resp.StatusCode)
	}
	resp = put("backup-db", map[string]any{"tags": []string{strings.Repeat("x", 65)}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT over-length = %d, want 422", resp.StatusCode)
	}

	// ── Control character → 422 (D6: tab/newline/etc. rejected) ──────────────────
	resp = put("backup-db", map[string]any{"tags": []string{"ok\ttab"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT control-char = %d, want 422", resp.StatusCode)
	}

	// ── Count cap: 30 accepted, 31 rejected (D6, dedupe happens before the cap) ───
	mk := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("t%d", i)
		}
		return out
	}
	resp = put("backup-db", map[string]any{"tags": mk(30)}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 30 tags = %d, want 200", resp.StatusCode)
	}
	var capped struct {
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&capped)
	resp.Body.Close()
	if len(capped.Tags) != 30 {
		t.Fatalf("30-tag set persisted %d tags, want 30", len(capped.Tags))
	}
	resp = put("backup-db", map[string]any{"tags": mk(31)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT 31 tags = %d, want 422", resp.StatusCode)
	}

	// ── Missing tags field → 422 (OpenAPI required:[tags]; not a silent clear) ────
	resp = put("backup-db", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT without tags field = %d, want 422", resp.StatusCode)
	}

	// ── Unknown script → 404 ─────────────────────────────────────────────────────
	resp = put("does-not-exist", map[string]any{"tags": []string{"x"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", resp.StatusCode)
	}

	// ── Clearing tags → 200 with [] ──────────────────────────────────────────────
	resp = put("backup-db", map[string]any{"tags": []string{}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	_ = pool.QueryRow(`SELECT tags FROM scripts WHERE name='backup-db'`).Scan(&raw)
	if raw != "[]" {
		t.Errorf("cleared tags = %q, want []", raw)
	}
}

// TestUpdateScriptTagsSlashName pins the load-bearing routing reason the PUT lives
// at /script-tags/{name...}: a sub-folder script's name is a repo-relative path
// with slashes. The generated client %2F-encodes them; the {name...} wildcard +
// PathValue must round-trip the full multi-segment name through the write path.
func TestUpdateScriptTagsSlashName(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scripts(name, run_type, script_path, content_hash, source_path, synced_at) VALUES(?,?,?,?,?,?)`,
		"ops-playbooks/site.yml", "ansible", "scripts/ops-playbooks/site.yml", "sha256:z", "scripts/ops-playbooks/site.yml", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	b, _ := json.Marshal(map[string]any{"tags": []string{"prod"}})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/script-tags/ops-playbooks%2Fsite.yml", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT slashed name: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT slashed name = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Name != "ops-playbooks/site.yml" || len(got.Tags) != 1 || got.Tags[0] != "prod" {
		t.Fatalf("slashed-name response = %+v, want name=ops-playbooks/site.yml tags=[prod]", got)
	}
	var raw string
	_ = pool.QueryRow(`SELECT tags FROM scripts WHERE name='ops-playbooks/site.yml'`).Scan(&raw)
	if raw != `["prod"]` {
		t.Errorf("stored tags = %q, want [\"prod\"]", raw)
	}
}
