package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpdateConfigTags exercises the operator-authored tag write endpoints added
// in migration 470 for the three cronomicon-owned Env Vars entities — variables,
// secrets, and SSH key credentials. Each mirrors the catalog tag contract
// (normalize / 422 / 404 / clear) but is permission-gated (ManageEnvVars for
// variables + secrets, ConfigureApp for credentials) and CSRF-guarded, and stores
// a plaintext JSON array that never touches the encrypted material. The shared
// normalizeTags caps are already proven by TestUpdateScriptTags; this verifies the
// wiring, gating, persistence, and read-back for each of the three new routes.
func TestUpdateConfigTags(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Minimal-but-realistic rows: the audit columns (created_by/last_modified_by/at)
	// are populated exactly as the real Create paths do, because the metadata scan
	// reads them into plain strings (NULL would error) — the tag write/read paths
	// otherwise touch only the tags column, never the (absent) encrypted material.
	seed(`INSERT INTO env_vars(id, key, value, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES('ev1','API_URL','https://x','tester','t','tester','t')`)
	seed(`INSERT INTO secrets(id, key, source, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES('s1','API_KEY','stored','tester','t','tester','t')`)
	seed(`INSERT INTO ssh_credentials(id, label, source, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES('c1','deploy-key','stored','tester','t','tester','t')`)

	client, csrf := devLogin(t, ts.URL)

	put := func(path string, body any, hdrCSRF bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if hdrCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", path, err)
		}
		return resp
	}
	getBody := func(path string) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return b
	}

	cases := []struct {
		name         string
		path         string // the {id} tag route for the seeded row
		badPath      string // a same-shape route for a non-existent id
		table        string // db table for the stored-tags assertion
		id           string
		listURL      string // entity list endpoint, for the read-back assertion
		auditCat     string // change_log/activity category
		target       string // change_log target (the key/label)
		wantActivity bool   // does a tag edit surface in the activity feed? (matches the entity's value-write path)
	}{
		{"env-var", "/api/v1/env-var-tags/ev1", "/api/v1/env-var-tags/nope", "env_vars", "ev1", "/api/v1/env-vars", "Env Vars", "API_URL", true},
		{"secret", "/api/v1/env-secret-tags/s1", "/api/v1/env-secret-tags/nope", "secrets", "s1", "/api/v1/env-secrets", "Secrets", "API_KEY", false},
		{"ssh-credential", "/api/v1/ssh-credential-tags/c1", "/api/v1/ssh-credential-tags/nope", "ssh_credentials", "c1", "/api/v1/ssh/credentials", "SSH Keys", "deploy-key", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ── CSRF guard ──────────────────────────────────────────────────────────
			resp := put(tc.path, map[string]any{"tags": []string{"prod"}}, false)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
			}

			// ── Valid full-replace + normalization ──────────────────────────────────
			// " Prod " trims, "" drops, "PROD" collapses into "Prod" (first casing wins).
			resp = put(tc.path, map[string]any{"tags": []string{" Prod ", "db", "", "PROD"}}, true)
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("PUT valid = %d, want 200 (%s)", resp.StatusCode, b)
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
			_ = pool.QueryRow(`SELECT tags FROM `+tc.table+` WHERE id=?`, tc.id).Scan(&raw)
			if raw != `["Prod","db"]` {
				t.Errorf("stored tags = %q, want %q", raw, `["Prod","db"]`)
			}

			// ── Audit: change_log always; activity only when the entity's value-write
			// path also emits activity (env vars yes; secrets/SSH no). Asserted here,
			// after exactly one tag write, before later writes add more rows. ────────
			var clog int
			_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category=? AND target=?`, tc.auditCat, tc.target).Scan(&clog)
			if clog == 0 {
				t.Errorf("%s tag write left no change_log row", tc.name)
			}
			var act int
			_ = pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind='config' AND category=? AND summary LIKE ?`,
				tc.auditCat, "%"+tc.target+"%").Scan(&act)
			if tc.wantActivity && act == 0 {
				t.Errorf("%s tag write emitted no activity row, but its value write does", tc.name)
			}
			if !tc.wantActivity && act != 0 {
				t.Errorf("%s tag write emitted %d activity rows; its value write is change_log-only", tc.name, act)
			}

			// ── Surfaces on the list read (proves the List scan includes tags) ──────
			if body := getBody(tc.listURL); !bytes.Contains(body, []byte(`"Prod"`)) {
				t.Errorf("%s list missing tag Prod: %s", tc.listURL, body)
			}

			// ── Missing tags field → 422 (required; not a silent clear) ─────────────
			resp = put(tc.path, map[string]any{}, true)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("PUT without tags field = %d, want 422", resp.StatusCode)
			}

			// ── Over-length tag → 422 (shared normalizeTags cap) ────────────────────
			resp = put(tc.path, map[string]any{"tags": []string{strings.Repeat("x", 65)}}, true)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("PUT over-length = %d, want 422", resp.StatusCode)
			}

			// ── Unknown id → 404 ────────────────────────────────────────────────────
			resp = put(tc.badPath, map[string]any{"tags": []string{"x"}}, true)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("PUT unknown = %d, want 404", resp.StatusCode)
			}

			// ── Clearing tags → 200 with [] ─────────────────────────────────────────
			resp = put(tc.path, map[string]any{"tags": []string{}}, true)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PUT clear = %d, want 200", resp.StatusCode)
			}
			_ = pool.QueryRow(`SELECT tags FROM `+tc.table+` WHERE id=?`, tc.id).Scan(&raw)
			if raw != "[]" {
				t.Errorf("cleared tags = %q, want []", raw)
			}
		})
	}
}

// TestUpdateConfigTagsPermGate pins the deliberate gating difference from the
// read-only catalog tag endpoints: these three carry the entity's own write
// permission — ManageEnvVars for variables + secrets, ConfigureApp for SSH key
// credentials — not the any-logged-in-user gate. With a valid CSRF token (so the
// shared CSRF check is satisfied and we isolate the perm gate), a viewer is 403
// while an admin gets past the gate into the handler (a 404 on the unseeded id,
// never 403). Mirrors TestRBACSshCredentialsWriteGate.
func TestUpdateConfigTagsPermGate(t *testing.T) {
	h := rbacServer(t)
	const body = `{"tags":["prod"]}`
	for _, path := range []string{
		"/api/v1/env-var-tags/ev1",
		"/api/v1/env-secret-tags/s1",
		"/api/v1/ssh-credential-tags/c1",
	} {
		for _, tc := range []struct {
			group         string
			wantForbidden bool
		}{
			{"rbac-viewers", true},
			{"rbac-admins", false},
		} {
			// An authenticated GET issues the CSRF cookie the PUT must echo.
			var csrf string
			for _, ck := range asGroup(t, h, "/api/v1/capabilities", tc.group).Result().Cookies() {
				if ck.Name == "cronomicon_csrf" {
					csrf = ck.Value
				}
			}
			if csrf == "" {
				t.Fatalf("%s: no CSRF cookie issued on GET", tc.group)
			}
			req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
			req.Header.Set("Remote-User", tc.group+"@example.com")
			req.Header.Set("Remote-Groups", tc.group)
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: "cronomicon_csrf", Value: csrf})
			req.Header.Set("X-CSRF-Token", csrf)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.wantForbidden && rec.Code != http.StatusForbidden {
				t.Errorf("viewer PUT %s = %d, want 403 (perm gate)", path, rec.Code)
			}
			if !tc.wantForbidden && rec.Code == http.StatusForbidden {
				t.Errorf("admin PUT %s = 403, want past the perm gate", path)
			}
		}
	}
}
