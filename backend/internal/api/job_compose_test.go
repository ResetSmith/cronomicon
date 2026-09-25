package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

// TestJobComposeCRUD exercises the in-app Job composition write API (A11, Phase 3):
// create an cronomicon-source job binding a Git script + a first-class schedule, the
// disjoint-namespace 409, the dangling-scriptRef 422, the CSRF guard, that git jobs
// are read-only (409), and that delete cascades the schedule bindings.
func TestJobComposeCRUD(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A Git script (the reusable body) + a first-class schedule + a git job named
	// 'reports' (to prove the git/cronomicon namespaces are disjoint).
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	seed(`INSERT INTO schedules(name, source, cron, content_hash, source_path, synced_at)
	      VALUES('nightly','git','0 0 2 * * *','sha256:bbb','schedules/nightly.yaml','t')`)
	seed(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('reports','git','bash','echo git','sha256:ccc','jobs/reports.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)

	post := func(method, url string, body any, hdrCSRF bool) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		if hdrCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// ── CSRF guard ────────────────────────────────────────────────────────────
	resp := post(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{"name": "x", "scriptRef": "backup-db", "scope": ""}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Create an cronomicon job ───────────────────────────────────────────────────
	body := map[string]any{
		"name":         "nightly-backup",
		"scriptRef":    "backup-db",
		"scope":        "Prod",
		"scheduleRefs": []string{"nightly"},
		"schedules":    []map[string]any{{"name": "extra", "cron": "0 0 6 * * *"}},
	}
	resp = post(http.MethodPost, ts.URL+"/api/v1/jobs", body, true)
	var created struct {
		ID      int64   `json:"id"`
		Name    string  `json:"name"`
		Source  string  `json:"source"`
		Type    string  `json:"type"`
		Command *string `json:"command"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Source != "cronomicon" {
		t.Errorf("created source = %q, want cronomicon", created.Source)
	}
	if created.Type != "bash" || created.Command == nil || *created.Command != "pg_dump mydb" {
		t.Errorf("denormalization failed: type=%q command=%v", created.Type, created.Command)
	}

	// definition_schedules carries both the ref + inline entry under owner_source='cronomicon'.
	var schedN int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_source='cronomicon' AND owner_kind='job' AND owner_name='nightly-backup'`).Scan(&schedN)
	if schedN != 2 {
		t.Errorf("cronomicon job schedule entries = %d, want 2 (ref + inline)", schedN)
	}

	// ── Disjoint namespace: an cronomicon 'reports' can coexist with the git one ────
	resp = post(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{"name": "reports", "scriptRef": "backup-db", "scope": ""}, true)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusCreated {
		t.Errorf("create cronomicon 'reports' (git 'reports' exists) = %d, want 201 (disjoint namespaces)", code)
	}

	// ── Duplicate cronomicon name → 409 ────────────────────────────────────────────
	resp = post(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{"name": "nightly-backup", "scriptRef": "backup-db", "scope": ""}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("duplicate cronomicon name = %d, want 409", code)
	}

	// ── Dangling scriptRef → 422 ────────────────────────────────────────────────
	resp = post(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{"name": "bad", "scriptRef": "ghost", "scope": ""}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusUnprocessableEntity {
		t.Errorf("dangling scriptRef = %d, want 422", code)
	}

	// ── Git job is read-only (409 on PUT) ───────────────────────────────────────
	var gitRowid int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE source='git' AND name='reports'`).Scan(&gitRowid)
	resp = post(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(gitRowid), map[string]any{"name": "reports", "scriptRef": "backup-db", "scope": ""}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("PUT git job = %d, want 409 (git is read-only in-app)", code)
	}

	// ── Delete the cronomicon job → 204 + soft delete ─────────────────────────────
	//
	// RH changed what delete MEANS. The row is stamped rather than removed and
	// its schedule bindings are deliberately left intact, because restoring from
	// the recycle bin is a single UPDATE that must lose nothing — bindings
	// destroyed here could not be brought back. What makes the job stop firing is
	// the reload query's deleted_at filter, not the absence of the rows.
	resp = post(http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(created.ID), nil, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}
	var live, binned, afterSched int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE source='cronomicon' AND name='nightly-backup' AND deleted_at IS NULL`).Scan(&live)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE source='cronomicon' AND name='nightly-backup' AND deleted_at IS NOT NULL`).Scan(&binned)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_source='cronomicon' AND owner_name='nightly-backup'`).Scan(&afterSched)
	if live != 0 {
		t.Errorf("job is still live after delete: %d", live)
	}
	if binned != 1 {
		t.Errorf("job rows in the recycle bin = %d, want 1", binned)
	}
	if afterSched == 0 {
		t.Error("schedule bindings were destroyed by a soft delete — an undelete could not restore them")
	}

	// It is gone from the catalog, and a second delete says so rather than
	// silently succeeding.
	resp = post(http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(created.ID), nil, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("second delete = %d, want 409 (already in the recycle bin)", code)
	}
}

// TestJobComposeAnsibleTargetHostValidation is the TG-4 authoring-time guard:
// an ansible job's targetHost is passed verbatim to `ansible --limit`, and
// AnsibleLimit silently DROPS any name carrying a pattern metacharacter — a
// dropped pin means no --limit at all, i.e. the run silently widens to the
// full inventory. writeComposedJob rejects a metachar targetHost with 422
// before any DB write; a plain host name still succeeds.
func TestJobComposeAnsibleTargetHostValidation(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('patch','ansible','- hosts: web\n','ssh','sha256:aaa','scripts/patch.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	post := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// ── Metachar targetHost → 422, no DB write ──────────────────────────────────
	resp := post(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "bad-pin", "scriptRef": "patch", "targetHost": "web[01:50]",
		"scope": "",
	})
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusUnprocessableEntity {
		t.Errorf("metachar targetHost = %d, want 422", code)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE source='cronomicon' AND name='bad-pin'`).Scan(&n)
	if n != 0 {
		t.Errorf("metachar targetHost job should not have been written, found %d rows", n)
	}

	// ── Plain host name → 201 ───────────────────────────────────────────────────
	resp = post(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "good-pin", "scriptRef": "patch", "targetHost": "web1",
		"scope": "",
	})
	var created struct {
		ID int64 `json:"id"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("plain targetHost create = %d, want 201", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	var th sql.NullString
	_ = pool.QueryRow(`SELECT target_host FROM jobs WHERE source='cronomicon' AND name='good-pin'`).Scan(&th)
	if !th.Valid || th.String != "web1" {
		t.Errorf("target_host = %v, want web1", th)
	}

	// ── Update path: PUT with a metachar targetHost also 422s ──────────────────
	resp = post(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID), map[string]any{
		"name": "good-pin", "scriptRef": "patch", "targetHost": "web:&staged",
		"scope": "",
	})
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusUnprocessableEntity {
		t.Errorf("PUT metachar targetHost = %d, want 422", code)
	}
	_ = pool.QueryRow(`SELECT target_host FROM jobs WHERE source='cronomicon' AND name='good-pin'`).Scan(&th)
	if th.String != "web1" {
		t.Errorf("target_host should be unchanged after rejected PUT, got %v", th)
	}
}
