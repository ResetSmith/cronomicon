package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// AF-1 — a composed job must STATE its agency assignment. The scope decides which
// agencies can see the job (scopeWhereFragment filters restricted callers to their
// granted scopes, and a NULL scope passes for everyone), so a job that is global
// merely because the field was never filled is invisible policy.
//
// What is pinned here is the distinction the pointer field exists for: SILENCE is
// refused, both ANSWERS are accepted. An explicit "" is a deliberate All and must
// keep working — this rule bans the accident, not global jobs — and it must still
// store as NULL, the convention the visibility filter reads.
func TestComposeRequiresAnExplicitScope(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
		 VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)

	send := func(method, url string, body any) *http.Response {
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
	readMsg := func(resp *http.Response) string {
		t.Helper()
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		return e.Message
	}

	// ── Absent field: refused, and the message names the escape ───────────────
	resp := send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "no-scope-stated", "scriptRef": "backup-db"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		resp.Body.Close()
		t.Fatalf("create without scope = %d, want 422", resp.StatusCode)
	}
	// A refusal that does not mention All would read as "global jobs are banned",
	// which is the opposite of the rule.
	if msg := readMsg(resp); !strings.Contains(msg, "All") {
		t.Fatalf("422 message does not name the All option: %q", msg)
	}

	// ── Explicit JSON null is silence too ─────────────────────────────────────
	resp = send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "null-scope", "scriptRef": "backup-db", "scope": nil})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		resp.Body.Close()
		t.Fatalf("create with null scope = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()

	// ── Explicit "" is a deliberate All: accepted, stored as NULL ─────────────
	resp = send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "deliberately-global", "scriptRef": "backup-db", "scope": ""})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with explicit All = %d (%s), want 201", resp.StatusCode, readMsg(resp))
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// NULL, not "": scopeWhereFragment tests `IS NULL` for the global pool, so an
	// empty string here would quietly drop the job out of every restricted
	// caller's list.
	var scope *string
	if err := pool.QueryRowContext(ctx,
		`SELECT scope FROM jobs WHERE name='deliberately-global' AND source='amadeus'`).Scan(&scope); err != nil {
		t.Fatalf("read back scope: %v", err)
	}
	if scope != nil {
		t.Fatalf("explicit All stored as %q, want NULL", *scope)
	}

	// ── A named scope is accepted unchanged ───────────────────────────────────
	resp = send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "scoped-job", "scriptRef": "backup-db", "scope": "Prod"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with named scope = %d (%s), want 201", resp.StatusCode, readMsg(resp))
	}
	resp.Body.Close()

	// ── The edit path enforces it too ─────────────────────────────────────────
	// An update that omits scope would otherwise blank a scoped job back to
	// global — the same accident, arriving through the other door.
	resp = send(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID),
		map[string]any{"name": "deliberately-global", "scriptRef": "backup-db"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		resp.Body.Close()
		t.Fatalf("update without scope = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()
}
