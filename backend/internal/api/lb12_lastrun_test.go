package api_test

import (
	"fmt"
	"testing"
	"time"
)

// TestWorkflowLastRunPopulated is the LB12 regression: the Workflows list +
// detail serializers must join workflow_runs so "Last Run" (lastRunAt) and the
// "Result" status reflect the most-recent run, instead of being null / hardcoded
// "idle". The match is source-qualified (workflow_source = ?) per A9, so runs are
// seeded with an explicit source rather than NULL.
func TestWorkflowLastRunPopulated(t *testing.T) {
	ts, db := newTestServer(t)
	now := time.Now().UTC()
	older := now.Add(-time.Hour).Format(time.RFC3339)
	newer := now.Format(time.RFC3339)

	// A workflow with two runs (source 'git'); the most-recent is a success.
	if _, err := db.Exec(`INSERT INTO workflows(name, source, steps, synced_at) VALUES('wf-ran','git','[]',?)`, newer); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	seedRun := func(id, status, createdAt string) {
		if _, err := db.Exec(`
			INSERT INTO workflow_runs(id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at)
			VALUES (?, 'wf-ran', 'git', ?, 'test', 'manual', ?)`, id, status, createdAt); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	seedRun("wr-old", "failure", older)
	seedRun("wr-new", "success", newer)

	// A workflow with no runs — lastRunAt null, status falls back to "idle".
	if _, err := db.Exec(`INSERT INTO workflows(name, source, steps, synced_at) VALUES('wf-norun','git','[]',?)`, newer); err != nil {
		t.Fatalf("seed norun workflow: %v", err)
	}

	client := devLoginClient(t, ts)

	type wfRow struct {
		ID        int64   `json:"id"`
		Name      string  `json:"name"`
		Status    string  `json:"status"`
		LastRunAt *string `json:"lastRunAt"`
	}

	var le struct {
		Items []wfRow `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/workflows", &le)
	byName := map[string]wfRow{}
	for _, it := range le.Items {
		byName[it.Name] = it
	}

	ran, ok := byName["wf-ran"]
	if !ok {
		t.Fatalf("wf-ran not in list response")
	}
	if ran.LastRunAt == nil || *ran.LastRunAt != newer {
		t.Errorf("wf-ran list lastRunAt = %v, want %q (most-recent run)", ran.LastRunAt, newer)
	}
	if ran.Status != "success" {
		t.Errorf("wf-ran list status = %q, want success (mapped from most-recent run)", ran.Status)
	}

	norun, ok := byName["wf-norun"]
	if !ok {
		t.Fatalf("wf-norun not in list response")
	}
	if norun.LastRunAt != nil {
		t.Errorf("wf-norun list lastRunAt = %q, want nil (no runs)", *norun.LastRunAt)
	}
	if norun.Status != "idle" {
		t.Errorf("wf-norun list status = %q, want idle (no runs)", norun.Status)
	}

	// Detail path must populate lastRunAt + status too.
	var detail wfRow
	getJSON(t, client, ts.URL+fmt.Sprintf("/api/v1/workflows/%d", ran.ID), &detail)
	if detail.LastRunAt == nil || *detail.LastRunAt != newer {
		t.Errorf("detail lastRunAt = %v, want %q", detail.LastRunAt, newer)
	}
	if detail.Status != "success" {
		t.Errorf("detail status = %q, want success", detail.Status)
	}
}
