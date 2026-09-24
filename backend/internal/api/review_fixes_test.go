package api_test

import (
	"testing"
	"time"
)

// TestWorkflowRunsStatusDangerFilter is the PP-H7-review regression: the
// /workflow-runs status filter sends the DISPLAY status ("danger"), which the
// server must reverse-map to the raw column values ('failure'/'killed'). Before
// the fix the filter compared 'danger' against the raw column and returned 0.
func TestWorkflowRunsStatusDangerFilter(t *testing.T) {
	ts, db := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	for i, st := range []string{"failure", "success", "killed"} {
		if _, err := db.Exec(`
			INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at)
			VALUES (?, 1, 'wf', ?, 'test', 'manual', ?)`,
			"wr-"+st, st, now); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	client := devLoginClient(t, ts)

	type env struct {
		TotalItems int `json:"totalItems"`
		Items      []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	var e env
	getJSON(t, client, ts.URL+"/api/v1/workflow-runs?status=danger", &e)
	// Both 'failure' and 'killed' map to the display status 'danger'.
	if e.TotalItems != 2 {
		t.Errorf("status=danger totalItems = %d, want 2 (failure + killed)", e.TotalItems)
	}
	for _, it := range e.Items {
		if it.Status != "danger" {
			t.Errorf("item status = %q, want danger", it.Status)
		}
	}

	// A single-value status still works (no reverse-map needed).
	var e2 env
	getJSON(t, client, ts.URL+"/api/v1/workflow-runs?status=success", &e2)
	if e2.TotalItems != 1 {
		t.Errorf("status=success totalItems = %d, want 1", e2.TotalItems)
	}
}
