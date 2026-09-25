package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// FX-D — a suppression is not a run, and every reader has to know it.
//
// A calendar veto, a Forbid refusal, a queue-full skip and a missed-fire marker
// are all real rows in `runs` with status='skipped'. They exist so a suppression
// is provable, which is the whole point of the calendar feature ("prove this job
// did not run on the holiday, deliberately"). But four readers took the newest
// row regardless of status, so a job that was stopped this morning reported it
// as its last run — and, because 'skipped' matches neither arm of the derived
// status expression, fell through to `idle`, dropping out of ?status=success and
// out of its own count.

func seedSkip(t *testing.T, pool *sql.DB, source, job, at, reason string) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO runs (id, job_name, job_source, run_type, status, queued_reason,
		                  triggered_by, trigger_kind, started_at, completed_at, created_at)
		VALUES (?, ?, ?, 'bash', 'skipped', ?, 'scheduler', 'scheduled', ?, ?, ?)`,
		"skip-"+at, job, source, reason, at, at, at); err != nil {
		t.Fatalf("seed skip: %v", err)
	}
}

func seedExecutedRun(t *testing.T, pool *sql.DB, source, job, at, status string) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO runs (id, job_name, job_source, run_type, status, duration_ms,
		                  triggered_by, trigger_kind, started_at, completed_at, created_at)
		VALUES (?, ?, ?, 'bash', ?, 4200, 'op@example.com', 'manual', ?, ?, ?)`,
		"run-"+at, job, source, status, at, at, at); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

type jobView struct {
	Status         string  `json:"status"`
	LastRunAt      *string `json:"lastRunAt"`
	LastSkippedAt  *string `json:"lastSkippedAt"`
	LastSkipReason *string `json:"lastSkipReason"`
	LastDurationMs *int64  `json:"lastDurationMs"`
}

// A job that succeeded yesterday and was suppressed today still reads as a job
// that succeeded — on the list, on the detail, and in the status filter.
func TestSuppressionDoesNotImpersonateTheLastRun(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "billing")

	seedExecutedRun(t, pool, "cronomicon", "billing", "2026-08-10T02:00:00Z", "success")
	seedSkip(t, pool, "cronomicon", "billing", "2026-08-11T02:00:00Z",
		`Skipped: suppressed by calendar "holidays" (Independence Day)`)

	t.Run("detail", func(t *testing.T) {
		code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil)
		if code != http.StatusOK {
			t.Fatalf("get job = %d: %s", code, body)
		}
		var jv jobView
		if err := json.Unmarshal(body, &jv); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if jv.LastRunAt == nil || *jv.LastRunAt != "2026-08-10T02:00:00Z" {
			t.Errorf("lastRunAt = %v, want the 08-10 SUCCESS — a suppression is not a run, and "+
				"reporting one as the last run makes a job that has not executed look current",
				derefStr(jv.LastRunAt))
		}
		if jv.LastDurationMs == nil {
			t.Error("lastDurationMs is nil — it followed the suppression, which has no duration")
		}
		// The suppression is separated, not discarded.
		if jv.LastSkippedAt == nil || *jv.LastSkippedAt != "2026-08-11T02:00:00Z" {
			t.Errorf("lastSkippedAt = %v, want the 08-11 suppression — it must still be "+
				"reachable, or the calendar's compliance story is lost", derefStr(jv.LastSkippedAt))
		}
		if jv.LastSkipReason == nil || *jv.LastSkipReason == "" {
			t.Error("lastSkipReason is empty — the operator cannot tell WHY it was stopped")
		}
		if jv.Status != "success" {
			t.Errorf("derived status = %q, want success — 'skipped' matches neither arm of the "+
				"status expression, so a suppression silently demoted the job to idle", jv.Status)
		}
	})

	t.Run("list and status filter", func(t *testing.T) {
		code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs?status=success", csrf, nil)
		if code != http.StatusOK {
			t.Fatalf("list = %d: %s", code, body)
		}
		var page struct {
			Items []struct {
				Name string `json:"name"`
				jobView
			} `json:"items"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		var found bool
		for _, it := range page.Items {
			if it.Name == "billing" {
				found = true
				if it.LastRunAt == nil || *it.LastRunAt != "2026-08-10T02:00:00Z" {
					t.Errorf("list lastRunAt = %v, want the 08-10 success", derefStr(it.LastRunAt))
				}
			}
		}
		if !found {
			t.Error("a job suppressed today dropped out of ?status=success — the same wrong " +
				"status also drives the pager count, so the page total was wrong with it")
		}
	})
}

// The control: a job whose ONLY rows are suppressions has genuinely never run,
// and must not borrow a last-run from anywhere.
func TestJobWithOnlySuppressionsHasNoLastRun(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "billing")
	seedSkip(t, pool, "cronomicon", "billing", "2026-08-11T02:00:00Z", "Skipped: the job is paused")

	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("get job = %d: %s", code, body)
	}
	var jv jobView
	_ = json.Unmarshal(body, &jv)
	if jv.LastRunAt != nil {
		t.Errorf("lastRunAt = %q for a job that has only ever been suppressed", *jv.LastRunAt)
	}
	if jv.LastSkippedAt == nil {
		t.Error("lastSkippedAt is nil — the suppression must still be visible somewhere")
	}
	if jv.Status != "idle" {
		t.Errorf("derived status = %q, want idle", jv.Status)
	}
}

// FX-D2 — the analytics total is what RAN. The UI renders it as
// "N runs in the last 30 days", so counting suppressions made a job stopped
// every day by a calendar read as fully active.
func TestAnalyticsTotalCountsRunsNotSuppressions(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "billing")

	// Inside the 30-day window, so the analytics filter admits both.
	seedExecutedRun(t, pool, "cronomicon", "billing", daysAgo(2), "success")
	seedSkip(t, pool, "cronomicon", "billing", daysAgo(1), "Skipped: the job is paused")

	code, body := rhDo(t, client, http.MethodGet,
		ts.URL+"/api/v1/analytics/runs?job=billing&source=cronomicon&window=30d", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("analytics = %d: %s", code, body)
	}
	var out struct {
		Total   int `json:"total"`
		Skipped int `json:"skipped"`
		Success int `json:"success"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 {
		t.Errorf("total = %d, want 1 — the suppression is not a run, and this number is "+
			"rendered to the operator as \"N runs\"", out.Total)
	}
	if out.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 — separating the buckets must not lose the suppression", out.Skipped)
	}
	if out.Success != 1 {
		t.Errorf("success = %d, want 1", out.Success)
	}
}

// daysAgo is an RFC3339 stamp inside any sane analytics window.
func daysAgo(n int) string {
	return time.Now().AddDate(0, 0, -n).UTC().Format(time.RFC3339)
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// The WORKFLOW twin. Written because the job half alone is exactly the shape
// that let the last several defects in this area survive: a guard applied to one
// twin reads as complete, and nothing was asserting the other.
func TestWorkflowSuppressionDoesNotImpersonateTheLastRun(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "step-one")

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "step-one"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}

	if _, err := pool.Exec(`
		INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at)
		VALUES ('wf-ok', 'release', 'cronomicon', 'success', 'op@example.com', 'manual', '2026-08-10T02:00:00Z')`); err != nil {
		t.Fatalf("seed workflow run: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, queued_reason,
		                           triggered_by, trigger_kind, created_at)
		VALUES ('wf-skip', 'release', 'cronomicon', 'skipped', 'Skipped: suppressed by calendar "weekends"',
		        'scheduler', 'scheduled', '2026-08-11T02:00:00Z')`); err != nil {
		t.Fatalf("seed workflow suppression: %v", err)
	}

	code, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/workflows", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list workflows = %d: %s", code, body)
	}
	var page struct {
		Items []struct {
			Name           string  `json:"name"`
			LastRunAt      *string `json:"lastRunAt"`
			LastSkippedAt  *string `json:"lastSkippedAt"`
			LastSkipReason *string `json:"lastSkipReason"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, it := range page.Items {
		if it.Name != "release" {
			continue
		}
		found = true
		if it.LastRunAt == nil || *it.LastRunAt != "2026-08-10T02:00:00Z" {
			t.Errorf("workflow lastRunAt = %v, want the 08-10 SUCCESS — the suppression is not a run",
				derefStr(it.LastRunAt))
		}
		if it.LastSkippedAt == nil || *it.LastSkippedAt != "2026-08-11T02:00:00Z" {
			t.Errorf("workflow lastSkippedAt = %v, want the 08-11 suppression — the job twin keeps "+
				"this evidence and the workflow twin must too", derefStr(it.LastSkippedAt))
		}
		if it.LastSkipReason == nil || *it.LastSkipReason == "" {
			t.Error("workflow lastSkipReason is empty — the operator cannot tell why it was stopped")
		}
	}
	if !found {
		t.Fatal("workflow 'release' missing from the list")
	}
}
