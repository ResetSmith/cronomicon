package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// FX-E — the declared-but-never-assigned fields, each driven end to end.
// Every one of these was in the spec, accepted by clients, and fed by nothing;
// the guard is the test that fails when the promise goes dead again.

// E1 — the sighting ledger's read surface. The write half shipped in v0.57.31
// as "the durable answer to 'the file landed, why did nothing happen?'" and no
// endpoint, UI or retention entry could reach it.
func TestFileSightingsAreReadable(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "ingest")

	seedSighting := func(id, path string, runID, refused any) {
		t.Helper()
		if _, err := pool.Exec(`
			INSERT INTO file_watch_sightings (id, job_source, job_name, path, size_bytes, mtime, runner_id, seen_at, run_id, refused_reason)
			VALUES (?, 'cronomicon', 'ingest', ?, 2048, '2026-08-12T01:59:00Z', 'r1', ?, ?, ?)`,
			id, path, "2026-08-12T02:00:0"+id[len(id)-1:]+"Z", runID, refused); err != nil {
			t.Fatalf("seed sighting: %v", err)
		}
	}
	seedSighting("sg-1", "/srv/in/a.csv", "run-123", nil)
	seedSighting("sg-2", "/srv/in/b.csv", nil, "Skipped: the job is paused")

	code, body := rhDo(t, client, http.MethodGet,
		ts.URL+"/api/v1/jobs/"+itoa(jobID)+"/file-sightings", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list sightings = %d: %s", code, body)
	}
	var page struct {
		TotalItems int `json:"totalItems"`
		Items      []struct {
			Path          string  `json:"path"`
			RunID         *string `json:"runId"`
			RefusedReason *string `json:"refusedReason"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.TotalItems != 2 || len(page.Items) != 2 {
		t.Fatalf("items = %d/%d, want 2", page.TotalItems, len(page.Items))
	}
	// Newest first: sg-2 (02:00:02) before sg-1 (02:00:01).
	if page.Items[0].Path != "/srv/in/b.csv" {
		t.Errorf("first item = %s, want the newest sighting", page.Items[0].Path)
	}
	if page.Items[0].RefusedReason == nil || *page.Items[0].RefusedReason == "" {
		t.Error("the refused sighting carries no reason — the question the ledger exists " +
			"to answer ('the file landed, why did nothing happen?') is still unanswerable")
	}
	if page.Items[1].RunID == nil || *page.Items[1].RunID != "run-123" {
		t.Error("the fired sighting does not link its run")
	}
}

// E3 — queuedReason on the job row: declared and documented since the spec was
// written ("e.g. 'waiting for terraform-capable runner'"), assigned by nothing.
func TestJobDetailLiftsTheQueuedReason(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "billing")

	if _, err := pool.Exec(`
		INSERT INTO runs (id, job_name, job_source, run_type, status, queued_reason,
		                  triggered_by, trigger_kind, created_at)
		VALUES ('q1', 'billing', 'cronomicon', 'bash', 'queued', 'no runner is online',
		        'op@example.com', 'manual', '2026-08-12T02:00:00Z')`); err != nil {
		t.Fatalf("seed queued run: %v", err)
	}

	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("get job = %d: %s", code, body)
	}
	var jv struct {
		QueuedReason *string `json:"queuedReason"`
	}
	_ = json.Unmarshal(body, &jv)
	if jv.QueuedReason == nil || *jv.QueuedReason != "no runner is online" {
		t.Errorf("queuedReason = %v, want the queued run's reason — the field was documented "+
			"with an example for a year while nothing assigned it", derefStr(jv.QueuedReason))
	}
}

// E8 — nextRunAt on the LIST row: documented since the spec was written,
// assigned only on the detail path.
func TestJobListCarriesNextRunAt(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
		"name": "billing", "scriptRef": "deploy.sh", "runType": "bash",
		"scope":     "",
		"schedules": []map[string]any{{"name": "nightly", "cron": "0 0 2 * * *"}},
	}); code != http.StatusCreated {
		t.Fatalf("create job = %d: %s", code, b)
	}

	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list jobs = %d: %s", code, body)
	}
	var page struct {
		Items []struct {
			Name      string  `json:"name"`
			NextRunAt *string `json:"nextRunAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var checked bool
	for _, it := range page.Items {
		if it.Name != "billing" {
			continue
		}
		checked = true
		if it.NextRunAt == nil || *it.NextRunAt == "" {
			t.Error("list nextRunAt is empty for a scheduled job — the spec has promised this " +
				"field on the list row since it was written, and only the detail kept it")
		}
	}
	if !checked {
		t.Fatal("job missing from list")
	}

	// The paused exclusion, matching the detail path: a paused job projects no
	// next run, because the scheduler will not fire it.
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs/1/pause", csrf, map[string]any{}); code >= 300 {
		t.Fatalf("pause = %d: %s", code, b)
	}
	code, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("relist = %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range page.Items {
		if it.Name == "billing" && it.NextRunAt != nil {
			t.Errorf("paused job projects nextRunAt = %q — the scheduler will not fire it, so "+
				"the list is promising a run that cannot happen", *it.NextRunAt)
		}
	}
}
