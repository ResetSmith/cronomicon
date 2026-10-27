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

// AF-1, as it stands since 2.5.3 (CA7) — every composed job names a scope.
// From AF-1 until 2.5.3 the pointer field told SILENCE (refused) from an
// explicit "" (a deliberate All, accepted and stored as NULL). Since 2.5.3 a
// job with no scope has nowhere to run, so "" is refused as silence is, on
// create and on edit, and the message names the shapes to use instead.
func TestComposeRequiresAnExplicitScope(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
		 VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)
	workInAgency(t, pool, client)
	copyGlobalDefsToWorkRepo(t, pool)

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
	refused := func(what string, resp *http.Response) {
		t.Helper()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			resp.Body.Close()
			t.Fatalf("%s = %d, want 422", what, resp.StatusCode)
		}
		// The refusal names the way out: a runner-local scope, or a hostless one.
		msg := readMsg(resp)
		for _, want := range []string{"every job names one of its agency's scopes", "ansible_connection=local", "lists no hosts"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s: the message %q does not say %q", what, msg, want)
			}
		}
	}

	refused("create without scope", send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "no-scope-stated", "scriptRef": "backup-db"}))
	refused("create with null scope", send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "null-scope", "scriptRef": "backup-db", "scope": nil}))
	refused("create with an empty scope", send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "deliberately-global", "scriptRef": "backup-db", "scope": ""}))
	refused("create with a blank scope", send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "blank-scope", "scriptRef": "backup-db", "scope": "  "}))
	var n int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE source='cronomicon'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d in-app jobs stored (%v), want none", n, err)
	}

	// ── A named scope is accepted unchanged ───────────────────────────────────
	resp := send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "scoped-job", "scriptRef": "backup-db", "scope": workScope})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with named scope = %d (%s), want 201", resp.StatusCode, readMsg(resp))
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// ── The edit path enforces it too: neither silence nor "" empties a scope ──
	refused("update without scope", send(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID),
		map[string]any{"name": "scoped-job", "scriptRef": "backup-db"}))
	refused("update to an empty scope", send(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID),
		map[string]any{"name": "scoped-job", "scriptRef": "backup-db", "scope": ""}))

	// ── A leftover with no scope (from before 2.5.3) is saved only with one ───
	if _, err := pool.ExecContext(ctx, `UPDATE jobs SET scope = NULL WHERE name = 'scoped-job' AND source = 'cronomicon'`); err != nil {
		t.Fatal(err)
	}
	refused("saving a leftover with no scope as it is", send(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID),
		map[string]any{"name": "scoped-job", "scriptRef": "backup-db", "scope": ""}))
	resp = send(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID),
		map[string]any{"name": "scoped-job", "scriptRef": "backup-db", "scope": workScope})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("giving the leftover a scope = %d (%s), want 200", resp.StatusCode, readMsg(resp))
	}
	resp.Body.Close()
	var scope sql.NullString
	if err := pool.QueryRowContext(ctx, `SELECT scope FROM jobs WHERE name = 'scoped-job' AND source = 'cronomicon'`).Scan(&scope); err != nil || scope.String != workScope {
		t.Fatalf("the leftover's scope = %v (%v), want %s", scope, err, workScope)
	}
}
