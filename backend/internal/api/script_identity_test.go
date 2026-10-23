package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The script routes address a script by NAME. Since migration 1290 (2.4.0,
// GR-5) a name is unique per repository, so a second repository may hold a
// script of the same name. Until a route can say which repository it means
// (Phase R6), it means Global's, and every statement behind it keys on that
// script's uid. This test puts a same-named script of another repository beside
// Global's and checks that no route reads it, counts it, or writes to it.
func TestScriptRoutesMeanGlobalsScript(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	str := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRowContext(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("query: %v\n%s", err, q)
		}
		return s
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("query: %v\n%s", err, q)
		}
		return n
	}

	// `deploy` twice: Global's and another repository's. And one script that
	// only the other repository has.
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, script, content_hash, synced_at) VALUES('s-global','global','deploy','bash','echo from-global','sha256:g',?)`, now)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, script, content_hash, synced_at) VALUES('s-other','repo-b','deploy','bash','echo from-other','sha256:o',?)`, now)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, script, content_hash, synced_at) VALUES('s-only-b','repo-b','only-b','bash','echo only-b','sha256:b',?)`, now)
	// A job on each `deploy`.
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid) VALUES('j-g','uses-global','git','bash',?,'deploy','s-global')`, now)
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid) VALUES('j-o','uses-other','cronomicon','bash',?,'deploy','s-other')`, now)
	exec(`INSERT INTO secrets(id, key, source, created_by, created_at, last_modified_by, last_modified_at) VALUES('s1','DB_PASS','stored','t',?,'t',?)`, now, now)

	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rdr io.Reader = bytes.NewReader(nil)
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		if method != http.MethodGet {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}

	// ── the detail, and who uses it ──────────────────────────────────────────
	code, raw := do(http.MethodGet, "/api/v1/scripts/deploy", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scripts/deploy = %d: %s", code, raw)
	}
	var detail struct {
		Script      *string  `json:"script"`
		ContentHash string   `json:"contentHash"`
		UsedBy      []string `json:"usedBy"`
	}
	_ = json.Unmarshal(raw, &detail)
	if detail.ContentHash != "sha256:g" || detail.Script == nil || *detail.Script != "echo from-global" {
		t.Errorf("GET /scripts/deploy returned hash %q: want Global's script", detail.ContentHash)
	}
	if len(detail.UsedBy) != 1 || detail.UsedBy[0] != "uses-global" {
		t.Errorf("usedBy = %v, want [uses-global]: a job of the other repository's deploy is not a user of this one", detail.UsedBy)
	}

	// ── the body ─────────────────────────────────────────────────────────────
	code, raw = do(http.MethodGet, "/api/v1/script-content/deploy", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), "from-global") || strings.Contains(string(raw), "from-other") {
		t.Errorf("GET /script-content/deploy = %d %s: want Global's body", code, raw)
	}
	code, raw = do(http.MethodGet, "/api/v1/script-reference-scan/deploy", nil)
	if code != http.StatusOK {
		t.Errorf("GET /script-reference-scan/deploy = %d: %s", code, raw)
	}

	// ── the list counts a script's users by its uid ──────────────────────────
	code, raw = do(http.MethodGet, "/api/v1/scripts?pageSize=50", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scripts = %d: %s", code, raw)
	}
	var list struct {
		Items []struct {
			Name        string `json:"name"`
			ContentHash string `json:"contentHash"`
			UsedByCount int    `json:"usedByCount"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &list)
	seen := 0
	for _, it := range list.Items {
		if it.Name != "deploy" {
			continue
		}
		seen++
		if it.UsedByCount != 1 {
			t.Errorf("list: deploy (%s) usedByCount = %d, want 1 (by name it would be 2)", it.ContentHash, it.UsedByCount)
		}
	}
	if seen != 2 {
		t.Errorf("list shows %d scripts named deploy, want both until the list is filtered by repository (R6)", seen)
	}

	// ── bindings are written to and read from Global's script only ───────────
	code, raw = do(http.MethodPut, "/api/v1/script-reference-bindings/deploy",
		map[string]any{"bindings": []map[string]string{{"kind": "secret", "name": "DB_PASS"}}})
	if code != http.StatusOK {
		t.Fatalf("PUT bindings = %d: %s", code, raw)
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid='s-global'`); n != 1 {
		t.Errorf("bindings under Global's script = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND (owner_uid IS NULL OR owner_uid != 's-global')`); n != 0 {
		t.Errorf("%d script bindings were filed somewhere other than under Global's script", n)
	}
	// The other repository's script gets a binding of its own, directly; the
	// route must not show it, and a save through the route must not remove it.
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at, owner_uid)
	      VALUES('script','','deploy','var','OTHERS','t',?,'s-other')`, now)
	code, raw = do(http.MethodGet, "/api/v1/script-reference-bindings/deploy", nil)
	var bs struct {
		Bindings []apiBinding `json:"bindings"`
	}
	_ = json.Unmarshal(raw, &bs)
	if code != http.StatusOK || len(bs.Bindings) != 1 || bs.Bindings[0].Name != "DB_PASS" {
		t.Errorf("GET bindings = %d %+v, want Global's one binding", code, bs.Bindings)
	}
	code, _ = do(http.MethodPut, "/api/v1/script-reference-bindings/deploy", map[string]any{"bindings": []any{}})
	if code != http.StatusOK {
		t.Fatalf("PUT empty bindings = %d", code)
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_uid='s-other'`); n != 1 {
		t.Errorf("clearing Global's script's bindings removed the other repository's (left %d)", n)
	}

	// ── tags ─────────────────────────────────────────────────────────────────
	code, raw = do(http.MethodPut, "/api/v1/script-tags/deploy", map[string]any{"tags": []string{"blue"}})
	if code != http.StatusOK {
		t.Fatalf("PUT tags = %d: %s", code, raw)
	}
	if got := str(`SELECT tags FROM scripts WHERE uid='s-global'`); got != `["blue"]` {
		t.Errorf("Global's script's tags = %s, want [\"blue\"]", got)
	}
	if got := str(`SELECT tags FROM scripts WHERE uid='s-other'`); got != `[]` {
		t.Errorf("the other repository's script's tags were changed: %s", got)
	}

	// ── a composed job is joined to Global's script ──────────────────────────
	code, raw = do(http.MethodPost, "/api/v1/jobs", map[string]any{"name": "composed", "scriptRef": "deploy", "scope": ""})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /jobs = %d: %s", code, raw)
	}
	if got := str(`SELECT COALESCE(script_uid,'') FROM jobs WHERE source='cronomicon' AND name='composed'`); got != "s-global" {
		t.Errorf("the composed job is joined to script %q, want Global's", got)
	}
	if got := str(`SELECT COALESCE(script,'') FROM jobs WHERE source='cronomicon' AND name='composed'`); got != "echo from-global" {
		t.Errorf("the composed job copied the body %q, want Global's", got)
	}

	// ── a name only another repository has is not found ──────────────────────
	for _, path := range []string{"/api/v1/scripts/only-b", "/api/v1/script-content/only-b",
		"/api/v1/script-reference-bindings/only-b", "/api/v1/script-reference-scan/only-b"} {
		if code, _ := do(http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: the route means Global's repository", path, code)
		}
	}
	if code, _ := do(http.MethodPut, "/api/v1/script-tags/only-b", map[string]any{"tags": []string{"x"}}); code != http.StatusNotFound {
		t.Errorf("PUT /script-tags/only-b = %d, want 404", code)
	}
	if code, _ := do(http.MethodPost, "/api/v1/jobs", map[string]any{"name": "composed-b", "scriptRef": "only-b", "scope": ""}); code != http.StatusUnprocessableEntity {
		t.Errorf("composing on a script only another repository has = %d, want 422", code)
	}
}
