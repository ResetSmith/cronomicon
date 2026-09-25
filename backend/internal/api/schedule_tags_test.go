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

// TestUpdateScheduleTags exercises the Schedules tags write API (tags-support.md):
// the CSRF guard, a valid full-replace with normalization, the length/count/control
// caps, the missing-field 422, the unknown-schedule 404, that the tags surface on
// the detail and list reads, the empty-array clear, AND the source-disambiguation
// invariant — a git and an cronomicon schedule can share a name, so the (source, name)
// key must tag exactly one of them.
func TestUpdateScheduleTags(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A git and an cronomicon schedule sharing the name "nightly".
	seed(`INSERT INTO schedules(name, source, cron, content_hash, source_path, synced_at)
	      VALUES('nightly','git','0 0 * * *','sha256:g','schedules/nightly.yaml','t')`)
	seed(`INSERT INTO schedules(name, source, cron, content_hash, synced_at)
	      VALUES('nightly','cronomicon','0 1 * * *','sha256:a','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	put := func(name, source string, body any, hdrCSRF bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		url := ts.URL + "/api/v1/schedule-tags/" + name
		if source != "" {
			url += "?source=" + source
		}
		req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if hdrCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s?source=%s: %v", name, source, err)
		}
		return resp
	}
	dbTags := func(source string) string {
		var raw string
		_ = pool.QueryRow(`SELECT tags FROM schedules WHERE source=? AND name='nightly'`, source).Scan(&raw)
		return raw
	}

	// ── CSRF guard ──────────────────────────────────────────────────────────────
	resp := put("nightly", "git", map[string]any{"tags": []string{"prod"}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Valid full-replace + normalization on the GIT row ────────────────────────
	resp = put("nightly", "git", map[string]any{"tags": []string{" Prod ", "cron", "", "PROD"}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200", resp.StatusCode)
	}
	var updated struct {
		Source string   `json:"source"`
		Tags   []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if updated.Source != "git" || len(updated.Tags) != 2 || updated.Tags[0] != "Prod" || updated.Tags[1] != "cron" {
		t.Fatalf("normalized response = %+v, want source=git tags=[Prod cron]", updated)
	}

	// ── Source disambiguation: only the git row was tagged ───────────────────────
	if got := dbTags("git"); got != `["Prod","cron"]` {
		t.Errorf("git tags = %q, want %q", got, `["Prod","cron"]`)
	}
	if got := dbTags("cronomicon"); got != "[]" {
		t.Errorf("cronomicon tags = %q, want [] (only the git row should change)", got)
	}

	// Tag the cronomicon row independently; the git row stays put.
	resp = put("nightly", "cronomicon", map[string]any{"tags": []string{"adhoc"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT cronomicon = %d, want 200", resp.StatusCode)
	}
	if got := dbTags("cronomicon"); got != `["adhoc"]` {
		t.Errorf("cronomicon tags = %q, want %q", got, `["adhoc"]`)
	}
	if got := dbTags("git"); got != `["Prod","cron"]` {
		t.Errorf("git tags changed by an cronomicon write: %q", got)
	}

	// ── Default source is git (no ?source) ───────────────────────────────────────
	resp = put("nightly", "", map[string]any{"tags": []string{"defaulted"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT default-source = %d, want 200", resp.StatusCode)
	}
	if got := dbTags("git"); got != `["defaulted"]` {
		t.Errorf("default-source write hit %q, want the git row", got)
	}
	if got := dbTags("cronomicon"); got != `["adhoc"]` {
		t.Errorf("default-source write disturbed the cronomicon row: %q", got)
	}

	// ── Surfaces on the detail read (per source) ─────────────────────────────────
	var detail struct {
		Tags []string `json:"tags"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedule-defs/nightly?source=cronomicon", &detail)
	if len(detail.Tags) != 1 || detail.Tags[0] != "adhoc" {
		t.Errorf("cronomicon detail tags = %v, want [adhoc]", detail.Tags)
	}

	// ── Surfaces on the list read (both rows present, correct tags) ──────────────
	var list struct {
		Items []struct {
			Name   string   `json:"name"`
			Source string   `json:"source"`
			Tags   []string `json:"tags"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedule-defs", &list)
	seen := map[string][]string{}
	for _, it := range list.Items {
		if it.Name == "nightly" {
			seen[it.Source] = it.Tags
		}
	}
	if len(seen["git"]) != 1 || seen["git"][0] != "defaulted" {
		t.Errorf("list git tags = %v, want [defaulted]", seen["git"])
	}
	if len(seen["cronomicon"]) != 1 || seen["cronomicon"][0] != "adhoc" {
		t.Errorf("list cronomicon tags = %v, want [adhoc]", seen["cronomicon"])
	}

	// ── Length boundary: 64 accepted, 65 rejected ────────────────────────────────
	resp = put("nightly", "git", map[string]any{"tags": []string{strings.Repeat("y", 64)}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 64-char = %d, want 200", resp.StatusCode)
	}
	resp = put("nightly", "git", map[string]any{"tags": []string{strings.Repeat("x", 65)}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT over-length = %d, want 422", resp.StatusCode)
	}

	// ── Control character → 422 ──────────────────────────────────────────────────
	resp = put("nightly", "git", map[string]any{"tags": []string{"ok\ttab"}}, true)
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
	resp = put("nightly", "git", map[string]any{"tags": mk(30)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 30 tags = %d, want 200", resp.StatusCode)
	}
	resp = put("nightly", "git", map[string]any{"tags": mk(31)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT 31 tags = %d, want 422", resp.StatusCode)
	}

	// ── Missing tags field → 422 ─────────────────────────────────────────────────
	resp = put("nightly", "git", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT without tags field = %d, want 422", resp.StatusCode)
	}

	// ── Unknown schedule → 404 ───────────────────────────────────────────────────
	resp = put("does-not-exist", "git", map[string]any{"tags": []string{"x"}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", resp.StatusCode)
	}

	// ── Clearing tags → 200 with [] ──────────────────────────────────────────────
	resp = put("nightly", "git", map[string]any{"tags": []string{}}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear = %d, want 200", resp.StatusCode)
	}
	if got := dbTags("git"); got != "[]" {
		t.Errorf("cleared git tags = %q, want []", got)
	}
}
