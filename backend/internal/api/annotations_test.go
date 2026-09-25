package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// AN-2 (the annotations plan) — the annotation read/write API.
//
// The write path is the tags skeleton, so most of what is asserted here is that
// it behaves like tags where it should (any logged-in user, CSRF-guarded, full
// replace, source-agnostic by rowid, writable on a binned definition) and unlike
// tags where the storage differs (an all-empty write DELETES the sidecar row
// rather than storing a blank one, and the notes never appear on a list row).

// seedAnnotatableJob inserts a git job WITH a uid.
//
// The uid is the point. jobs.uid is the primary key as of 1050 but SQLite still
// permits NULL in a non-INTEGER primary-key column, so a seed that omits it
// produces a row the annotation path cannot address — every assertion below
// would then pass against a 404 and prove nothing. Production writers (sync and
// the composer) always stamp one.
func seedAnnotatableJob(t *testing.T, pool *sql.DB, name, uid, scope string) string {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs(name, source, uid, run_type, command, scope, content_hash, source_path, synced_at)
		VALUES(?, 'git', ?, 'bash', 'echo hi', ?, 'sha256:aaa', 'jobs/'||?||'.yaml', 't')`,
		name, uid, scope, name); err != nil {
		t.Fatalf("seed job %s: %v", name, err)
	}
	var rowid int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE uid = ?`, uid).Scan(&rowid); err != nil {
		t.Fatalf("rowid for %s: %v", name, err)
	}
	return strconv.FormatInt(rowid, 10)
}

func seedAnnotatableWorkflow(t *testing.T, pool *sql.DB, name, uid string) string {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO workflows(name, source, uid, steps, synced_at)
		VALUES(?, 'git', ?, '[]', 't')`, name, uid); err != nil {
		t.Fatalf("seed workflow %s: %v", name, err)
	}
	var rowid int64
	if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE uid = ?`, uid).Scan(&rowid); err != nil {
		t.Fatalf("rowid for %s: %v", name, err)
	}
	return strconv.FormatInt(rowid, 10)
}

// annotationResp is the subset of a job/workflow row this band adds.
type annotationResp struct {
	Critical bool   `json:"critical"`
	Contact  string `json:"contact"`
	Notes    string `json:"notes"`
	NotesBy  string `json:"notesBy"`
	NotesAt  string `json:"notesAt"`
}

func TestUpdateJobAnnotation(t *testing.T) {
	ts, pool := newTestServer(t)
	id := seedAnnotatableJob(t, pool, "backup-db", "uid-backup", "Prod")

	client, csrf := devLoginWithCSRF(t, ts)
	put := func(jobID string, body any, withCSRF bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/job-annotation/"+jobID, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if withCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", jobID, err)
		}
		return resp
	}
	countRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM annotations`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// ── CSRF guard ──────────────────────────────────────────────────────────────
	resp := put(id, map[string]any{"critical": true}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", resp.StatusCode)
	}

	// ── Round-trip ──────────────────────────────────────────────────────────────
	resp = put(id, map[string]any{
		"critical": true,
		"contact":  "  dba-oncall@corp.example  ", // trimmed server-side
		"notes":    "Restores from the 02:00 dump.\nCheck disk before re-running.",
	}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT valid = %d, want 200", resp.StatusCode)
	}
	var updated annotationResp
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if !updated.Critical || updated.Contact != "dba-oncall@corp.example" {
		t.Fatalf("response critical=%v contact=%q, want true / trimmed address", updated.Critical, updated.Contact)
	}
	if !strings.Contains(updated.Notes, "Check disk") {
		t.Errorf("response notes = %q, want the newline preserved", updated.Notes)
	}
	// Attribution is server-assigned and must come back on the write response —
	// the UI renders "Updated by X" straight from it without a second fetch.
	if updated.NotesBy == "" || updated.NotesAt == "" {
		t.Errorf("notesBy=%q notesAt=%q, want both server-assigned", updated.NotesBy, updated.NotesAt)
	}

	// ── The client cannot forge attribution ─────────────────────────────────────
	resp = put(id, map[string]any{
		"critical": true, "contact": "x@y", "notes": "n",
		"notesBy": "someone-else@corp.example", "notesAt": "1999-01-01T00:00:00Z",
	}, true)
	var forged annotationResp
	_ = json.NewDecoder(resp.Body).Decode(&forged)
	resp.Body.Close()
	if forged.NotesBy == "someone-else@corp.example" || strings.HasPrefix(forged.NotesAt, "1999") {
		t.Errorf("client-supplied attribution was accepted: by=%q at=%q", forged.NotesBy, forged.NotesAt)
	}

	// ── Detail read carries everything ──────────────────────────────────────────
	resp = put(id, map[string]any{"critical": true, "contact": "dba@corp.example", "notes": "the note"}, true)
	resp.Body.Close()
	var detail annotationResp
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+id, &detail)
	if !detail.Critical || detail.Contact != "dba@corp.example" || detail.Notes != "the note" {
		t.Errorf("detail = %+v, want the full annotation", detail)
	}

	// ── List row carries the chip and contact, NOT the notes ────────────────────
	// The omission is deliberate (a 4KB note per row times a page size), so it is
	// asserted rather than left to whoever next edits the query.
	var list struct {
		Items []struct {
			Name string `json:"name"`
			annotationResp
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/jobs", &list)
	found := false
	for _, it := range list.Items {
		if it.Name != "backup-db" {
			continue
		}
		found = true
		if !it.Critical || it.Contact != "dba@corp.example" {
			t.Errorf("list row critical=%v contact=%q, want the annotation subset", it.Critical, it.Contact)
		}
		if it.Notes != "" {
			t.Errorf("list row carries notes = %q, want them detail-only", it.Notes)
		}
	}
	if !found {
		t.Error("backup-db missing from list")
	}

	// ── Caps: boundary accepted, one over rejected ──────────────────────────────
	resp = put(id, map[string]any{"notes": strings.Repeat("n", 4096)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 4096-char notes = %d, want 200", resp.StatusCode)
	}
	resp = put(id, map[string]any{"notes": strings.Repeat("n", 4097)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT over-cap notes = %d, want 400", resp.StatusCode)
	}
	resp = put(id, map[string]any{"contact": strings.Repeat("c", 256)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 256-char contact = %d, want 200", resp.StatusCode)
	}
	resp = put(id, map[string]any{"contact": strings.Repeat("c", 257)}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT over-cap contact = %d, want 400", resp.StatusCode)
	}

	// ── Unknown job → 404 ───────────────────────────────────────────────────────
	resp = put("999999", map[string]any{"notes": "x"}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", resp.StatusCode)
	}

	// ── All-empty write DELETES the row ─────────────────────────────────────────
	// Asserted on the TABLE, not the response shape: a blank row would serialize
	// identically to no row, so only the count can tell them apart — and the
	// difference matters, because a stored blank would render as "annotated with
	// nothing, by someone, at some time".
	if countRows() != 1 {
		t.Fatalf("expected exactly one annotation before the clear, got %d", countRows())
	}
	resp = put(id, map[string]any{"critical": false, "contact": "", "notes": ""}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear = %d, want 200", resp.StatusCode)
	}
	if n := countRows(); n != 0 {
		t.Errorf("annotations rows after an all-empty write = %d, want 0 (a blank row is not the same as none)", n)
	}
	// Whitespace-only counts as empty too — trimming happens before the test.
	resp = put(id, map[string]any{"contact": "   ", "notes": "  \n "}, true)
	resp.Body.Close()
	if n := countRows(); n != 0 {
		t.Errorf("whitespace-only write stored %d rows, want 0", n)
	}
}

// TestUpdateWorkflowAnnotation — the workflow twin. Shorter than the job test on
// purpose: the shared prefix is the same code, so what is worth asserting here
// is that the workflow route exists, is gated, round-trips, and reaches the
// workflow half of the sidecar rather than the job half.
func TestUpdateWorkflowAnnotation(t *testing.T) {
	ts, pool := newTestServer(t)
	wfID := seedAnnotatableWorkflow(t, pool, "deploy", "uid-deploy")
	// A job sharing the workflow's uid string: the two halves are keyed by
	// (kind, uid), so a write to one must not surface on the other.
	jobID := seedAnnotatableJob(t, pool, "deploy", "uid-deploy-job", "Prod")

	client, csrf := devLoginWithCSRF(t, ts)

	code, body := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/workflow-annotation/"+wfID, csrf,
		map[string]any{"critical": true, "contact": "#platform", "notes": "release train"})
	if code != http.StatusOK {
		t.Fatalf("PUT workflow annotation = %d: %s", code, body)
	}
	var wr annotationResp
	_ = json.Unmarshal(body, &wr)
	if !wr.Critical || wr.Contact != "#platform" || wr.Notes != "release train" {
		t.Errorf("workflow response = %+v, want the written annotation", wr)
	}
	if wr.NotesBy == "" || wr.NotesAt == "" {
		t.Errorf("workflow attribution missing: by=%q at=%q", wr.NotesBy, wr.NotesAt)
	}

	// The same-uid job is untouched.
	var jd annotationResp
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+jobID, &jd)
	if jd.Critical || jd.Notes != "" {
		t.Errorf("job picked up the workflow's annotation: %+v", jd)
	}

	// Detail + list reads of the workflow half.
	var detail annotationResp
	getJSON(t, client, ts.URL+"/api/v1/workflows/"+wfID, &detail)
	if detail.Notes != "release train" {
		t.Errorf("workflow detail notes = %q", detail.Notes)
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
			annotationResp
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/workflows", &list)
	for _, it := range list.Items {
		if it.Name == "deploy" {
			if !it.Critical || it.Contact != "#platform" {
				t.Errorf("workflow list row = %+v, want the annotation subset", it.annotationResp)
			}
			if it.Notes != "" {
				t.Errorf("workflow list row carries notes = %q, want them detail-only", it.Notes)
			}
		}
	}

	// Unknown workflow → 404.
	if code, _ := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/workflow-annotation/999999", csrf,
		map[string]any{"notes": "x"}); code != http.StatusNotFound {
		t.Errorf("PUT unknown workflow = %d, want 404", code)
	}
}

// TestAnnotationWritableOnBinnedDefinition pins AN-Q6.
//
// Annotations follow tags, which are a documented exemption from the "every
// acting route refuses a binned definition" rule (FX-Q5, see
// deleted_filter_sites_test.go's exemption block). This is why neither PUT
// appears in binnedDefinitionRoutes(): a binned definition KEEPS its annotation
// (1060's triggers are AFTER DELETE; the bin is an UPDATE), so a note you can
// read but not correct would be the odd state — and "why did this get binned" is
// exactly the note someone wants to write at that moment.
func TestAnnotationWritableOnBinnedDefinition(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	jobID := createRHJob(t, ts, client, csrf, "billing")
	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "billing"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)

	// Annotate both while live, then bin both.
	if c, b := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/job-annotation/"+itoa(jobID), csrf,
		map[string]any{"critical": true, "notes": "live note"}); c != http.StatusOK {
		t.Fatalf("annotate live job = %d: %s", c, b)
	}
	if c, _ := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil); c != http.StatusNoContent {
		t.Fatalf("bin job = %d", c)
	}
	if c, _ := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/workflows/"+itoa(wf.ID), csrf, nil); c != http.StatusNoContent {
		t.Fatalf("bin workflow = %d", c)
	}

	// The bin kept the note (the AN-1 lifecycle contract, observed through the API).
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM annotations WHERE owner_kind='job'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("binning the job dropped its annotation (rows=%d, want 1)", n)
	}

	// And it can still be corrected while binned.
	if c, b := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/job-annotation/"+itoa(jobID), csrf,
		map[string]any{"critical": true, "contact": "dba@corp.example", "notes": "binned 2026-08-14, superseded by billing-v2"}); c != http.StatusOK {
		t.Errorf("PUT annotation on a BINNED job = %d, want 200 (AN-Q6): %s", c, b)
	}
	if c, b := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/workflow-annotation/"+itoa(wf.ID), csrf,
		map[string]any{"notes": "binned with its job"}); c != http.StatusOK {
		t.Errorf("PUT annotation on a BINNED workflow = %d, want 200 (AN-Q6): %s", c, b)
	}
}

// TestAnnotationPerTwinOverTheAPI is TestAnnotationPerTwin's API-level sibling
// (the db-package test proves the storage keeps twins apart; this proves the
// HTTP path addresses them separately). Two same-named cronomicon jobs, one
// annotated: the other must not inherit it.
func TestAnnotationPerTwinOverTheAPI(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	seed := func(uid, scope string) string {
		t.Helper()
		if _, err := pool.Exec(`
			INSERT INTO jobs(name, source, uid, run_type, command, scope, synced_at)
			VALUES('backup','cronomicon',?,'bash','echo hi',?,'t')`, uid, scope); err != nil {
			t.Fatalf("seed twin %s: %v", uid, err)
		}
		var rowid int64
		if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE uid = ?`, uid).Scan(&rowid); err != nil {
			t.Fatalf("rowid: %v", err)
		}
		return strconv.FormatInt(rowid, 10)
	}
	a := seed("uid-fin", "Finance")
	b := seed("uid-plat", "Platform")

	if c, body := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/job-annotation/"+a, csrf,
		map[string]any{"critical": true, "contact": "finance-dba@corp.example"}); c != http.StatusOK {
		t.Fatalf("annotate twin A = %d: %s", c, body)
	}

	var da, db annotationResp
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+a, &da)
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+b, &db)
	if !da.Critical || da.Contact != "finance-dba@corp.example" {
		t.Errorf("twin A lost its annotation: %+v", da)
	}
	if db.Critical || db.Contact != "" {
		t.Errorf("twin B inherited twin A's annotation: %+v — the path resolved by name", db)
	}
}
