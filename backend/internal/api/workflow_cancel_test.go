package api_test

import (
	"net/http"
	"testing"
)

// TestCancelWorkflowRunEndpoint covers WB-S2 at the HTTP layer: cancelling a
// running workflow run flags it cancelled, marks not-started (queued) children
// skipped, surfaces the display status 'cancelled', and 409s a second cancel.
func TestCancelWorkflowRunEndpoint(t *testing.T) {
	ts, pool := newTestServer(t)
	const ts0 = "2026-01-01T00:00:00Z"
	const wr = "wr-cancel-1"
	if _, err := pool.Exec(
		`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at)
		 VALUES (?, 1, 'wf', 'running', 't@example.com', 'manual', ?)`, wr, ts0); err != nil {
		t.Fatalf("seed wf run: %v", err)
	}
	// One queued child (not started) + one running child (in flight).
	for _, c := range []struct{ id, status string }{{"r-queued", "queued"}, {"r-running", "running"}} {
		if _, err := pool.Exec(
			`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, workflow_run_id)
			 VALUES (?, 'j', 'bash', ?, 't@example.com', 'workflow', ?, ?)`, c.id, c.status, ts0, wr); err != nil {
			t.Fatalf("seed child %s: %v", c.id, err)
		}
	}

	client, csrf := devLoginWithCSRF(t, ts)
	cancel := func() int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/workflows/runs/"+wr+"/cancel", nil)
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := cancel(); code != http.StatusAccepted {
		t.Fatalf("first cancel = %d, want 202", code)
	}

	var cancelled int
	_ = pool.QueryRow(`SELECT cancelled FROM workflow_runs WHERE id=?`, wr).Scan(&cancelled)
	if cancelled != 1 {
		t.Errorf("cancelled flag = %d, want 1", cancelled)
	}
	var queuedStatus, runningStatus string
	_ = pool.QueryRow(`SELECT status FROM runs WHERE id='r-queued'`).Scan(&queuedStatus)
	_ = pool.QueryRow(`SELECT status FROM runs WHERE id='r-running'`).Scan(&runningStatus)
	if queuedStatus != "skipped" {
		t.Errorf("queued child status = %q, want skipped", queuedStatus)
	}
	if runningStatus != "running" {
		t.Errorf("running child status = %q, want running (soft cancel lets it finish)", runningStatus)
	}

	// The detail serializer surfaces the display status 'cancelled'.
	var detail struct {
		Status    string `json:"status"`
		Cancelled bool   `json:"cancelled"`
	}
	getJSON(t, client, ts.URL+"/api/v1/workflow-runs/"+wr, &detail)
	if detail.Status != "cancelled" || !detail.Cancelled {
		t.Errorf("detail status=%q cancelled=%v, want cancelled/true", detail.Status, detail.Cancelled)
	}

	// A second cancel is a no-op conflict (already cancelled).
	if code := cancel(); code != http.StatusConflict {
		t.Errorf("second cancel = %d, want 409", code)
	}
}
