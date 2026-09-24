package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

// TestScheduleComposeCRUD exercises the in-app first-class Schedule authoring write
// API (schedule-builder.md): create an amadeus schedule, the bad-cron 422, the
// duplicate-name 409, the CSRF guard, that git schedules are read-only (409), that an
// edit PROPAGATES to a referencing job's definition_schedules row (the D1c crux), that
// delete-when-referenced blocks with 409, and that ?force detaches cleanly while
// resyncing the legacy display column.
func TestScheduleComposeCRUD(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A Git script (so the compose POST /jobs can bind a body) + a git schedule named
	// 'gitsched' (to prove git rows are read-only in-app).
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	seed(`INSERT INTO schedules(name, source, cron, content_hash, source_path, synced_at)
	      VALUES('gitsched','git','0 0 1 * * *','sha256:bbb','schedules/gitsched.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)

	do := func(method, url string, body any, hdrCSRF bool) *http.Response {
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

	// ── CSRF guard ──────────────────────────────────────────────────────────────
	resp := do(http.MethodPost, ts.URL+"/api/v1/schedule-defs", map[string]any{"name": "x", "cron": "0 0 2 * * *"}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Bad cron → 422 ────────────────────────────────────────────────────────────
	resp = do(http.MethodPost, ts.URL+"/api/v1/schedule-defs", map[string]any{"name": "bad", "cron": "bogus"}, true)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusUnprocessableEntity {
		t.Errorf("bad cron = %d, want 422", code)
	}

	// ── Create an amadeus schedule ────────────────────────────────────────────────
	resp = do(http.MethodPost, ts.URL+"/api/v1/schedule-defs",
		map[string]any{"name": "nightly", "cron": "0 0 2 * * *", "description": "nightly window", "env": map[string]string{"STAGE": "prod"}}, true)
	var created struct {
		Name        string            `json:"name"`
		Source      string            `json:"source"`
		Cron        string            `json:"cron"`
		Env         map[string]string `json:"env"`
		ContentHash string            `json:"contentHash"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Source != "amadeus" || created.Cron != "0 0 2 * * *" {
		t.Errorf("created = %+v, want source=amadeus cron='0 0 2 * * *'", created)
	}
	if created.ContentHash == "" || created.Env["STAGE"] != "prod" {
		t.Errorf("created content_hash/env not persisted: %+v", created)
	}

	// ── Duplicate amadeus name → 409 ───────────────────────────────────────────────
	resp = do(http.MethodPost, ts.URL+"/api/v1/schedule-defs", map[string]any{"name": "nightly", "cron": "0 0 3 * * *"}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("duplicate amadeus name = %d, want 409", code)
	}

	// ── Git schedule is read-only (409 on PUT + DELETE) ─────────────────────────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/schedule-defs/gitsched", map[string]any{"cron": "0 0 4 * * *"}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("PUT git schedule = %d, want 409", code)
	}
	resp = do(http.MethodDelete, ts.URL+"/api/v1/schedule-defs/gitsched", nil, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("DELETE git schedule = %d, want 409", code)
	}

	// ── Unknown name → 404 ──────────────────────────────────────────────────────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/schedule-defs/ghost", map[string]any{"cron": "0 0 2 * * *"}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNotFound {
		t.Errorf("PUT unknown schedule = %d, want 404", code)
	}

	// ── A referencing amadeus job binds 'nightly' (so we can prove propagation) ─────
	resp = do(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "backup-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"nightly"}}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusCreated {
		t.Fatalf("create referencing job = %d, want 201", code)
	}
	// The binding expanded with an exact source_ref and the legacy mirror = the cron.
	var refCron, mirror string
	var srcRef sql.NullString
	_ = pool.QueryRow(`SELECT cron, source_ref FROM definition_schedules WHERE owner_name='backup-job' AND name='nightly'`).Scan(&refCron, &srcRef)
	if refCron != "0 0 2 * * *" || !srcRef.Valid || srcRef.String != "nightly" {
		t.Fatalf("binding cron=%q source_ref=%v, want '0 0 2 * * *' / 'nightly'", refCron, srcRef)
	}
	_ = pool.QueryRow(`SELECT schedule FROM jobs WHERE source='amadeus' AND name='backup-job'`).Scan(&mirror)
	if mirror != "0 0 2 * * *" {
		t.Errorf("legacy mirror after bind = %q, want '0 0 2 * * *'", mirror)
	}

	// ── Edit 'nightly' → propagates to the referencing job (D1c) ────────────────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/schedule-defs/nightly", map[string]any{"cron": "0 0 5 * * *"}, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusOK {
		t.Fatalf("edit = %d, want 200", code)
	}
	_ = pool.QueryRow(`SELECT cron FROM definition_schedules WHERE owner_name='backup-job' AND name='nightly'`).Scan(&refCron)
	if refCron != "0 0 5 * * *" {
		t.Errorf("propagation failed: referencing job's definition_schedules cron = %q, want '0 0 5 * * *'", refCron)
	}
	_ = pool.QueryRow(`SELECT schedule FROM jobs WHERE source='amadeus' AND name='backup-job'`).Scan(&mirror)
	if mirror != "0 0 5 * * *" {
		t.Errorf("legacy mirror not resynced on edit = %q, want '0 0 5 * * *'", mirror)
	}

	// ── Delete blocked while referenced → 409 ───────────────────────────────────────
	resp = do(http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly", nil, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Fatalf("delete while referenced = %d, want 409", code)
	}
	var stillThere int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM schedules WHERE source='amadeus' AND name='nightly'`).Scan(&stillThere)
	if stillThere != 1 {
		t.Errorf("blocked delete still removed the schedule (%d)", stillThere)
	}

	// ── Forced delete → detaches the binding + resyncs the legacy mirror to NULL ────
	resp = do(http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly?force=true", nil, true)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNoContent {
		t.Fatalf("forced delete = %d, want 204", code)
	}
	// RH: the catalog row is soft-deleted (recoverable), but its ref-expanded
	// runtime entries are still detached — a binned schedule must stop firing its
	// referrers, and they are rebuilt from the catalog row on restore.
	var schedLive, schedBinned, bindN int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM schedules WHERE source='amadeus' AND name='nightly' AND deleted_at IS NULL`).Scan(&schedLive)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM schedules WHERE source='amadeus' AND name='nightly' AND deleted_at IS NOT NULL`).Scan(&schedBinned)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE source_ref='nightly'`).Scan(&bindN)
	if schedLive != 0 || bindN != 0 {
		t.Errorf("forced delete left residue: live schedule=%d bindings=%d, want 0/0", schedLive, bindN)
	}
	if schedBinned != 1 {
		t.Errorf("schedule rows in the recycle bin = %d, want 1", schedBinned)
	}
	var mirrorNull sql.NullString
	_ = pool.QueryRow(`SELECT schedule FROM jobs WHERE source='amadeus' AND name='backup-job'`).Scan(&mirrorNull)
	if mirrorNull.Valid {
		t.Errorf("legacy mirror not cleared after detach: %q, want NULL (no remaining entries)", mirrorNull.String)
	}
}
