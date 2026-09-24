package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// FX-F1 — the workflows LIST projects next-run through the full schedule spec.
//
// It used to select the cron column alone and project through bare
// cronutil.Next, making it the one surface disagreeing with the engine: an
// interval-mode entry showed NO next run (Next("") fails), and a windowed cron
// showed a PHANTOM fire before its window opened. The workflow DETAIL was
// already right (it routes through loadSchedules), so list and detail
// disagreed about the same entry.

func createWorkflowWithSchedule(t *testing.T, ts, name string, client *http.Client, csrf string, schedule map[string]any) {
	t.Helper()
	code, body := rhDo(t, client, http.MethodPost, ts+"/api/v1/workflows", csrf, map[string]any{
		"name":      name,
		"steps":     []map[string]any{{"type": "job", "name": "step-one"}},
		"schedules": []map[string]any{schedule},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow %s = %d: %s", name, code, body)
	}
}

func TestWorkflowListProjectsTheFullScheduleSpec(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "step-one")

	// An interval-mode schedule: no cron at all.
	anchor := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	createWorkflowWithSchedule(t, ts.URL, "interval-wf", client, csrf, map[string]any{
		"name": "hourly", "interval": "1h", "startAt": anchor,
	})
	// A windowed cron whose window opens next YEAR: the next fire is inside the
	// window, never before it.
	windowOpen := time.Now().UTC().AddDate(1, 0, 0)
	createWorkflowWithSchedule(t, ts.URL, "windowed-wf", client, csrf, map[string]any{
		"name": "nightly", "cron": "0 0 2 * * *", "startAt": windowOpen.Format(time.RFC3339),
	})

	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/workflows", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list workflows = %d: %s", code, body)
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
	byName := map[string]*string{}
	for _, it := range page.Items {
		byName[it.Name] = it.NextRunAt
	}

	if at, ok := byName["interval-wf"]; !ok {
		t.Fatal("interval-wf missing from list")
	} else if at == nil || *at == "" {
		t.Error("an interval-mode workflow schedule projects NO next run on the list — " +
			"bare cronutil.Next cannot parse an empty cron, and the detail page disagrees")
	} else if next, err := time.Parse(time.RFC3339, *at); err != nil || time.Until(next) > 61*time.Minute {
		t.Errorf("interval-wf nextRunAt = %s — want within the next hour (the 1h interval)", *at)
	}

	if at, ok := byName["windowed-wf"]; !ok {
		t.Fatal("windowed-wf missing from list")
	} else if at == nil {
		t.Error("a windowed cron projects no next run at all — the window defers it, not deletes it")
	} else if next, err := time.Parse(time.RFC3339, *at); err != nil || next.Before(windowOpen.Add(-time.Hour)) {
		t.Errorf("windowed-wf nextRunAt = %s, before its window opens (%s) — the phantom fire "+
			"the window exists to prevent", *at, windowOpen.Format(time.RFC3339))
	}
}
