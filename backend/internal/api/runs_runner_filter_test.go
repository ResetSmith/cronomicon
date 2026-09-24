package api_test

import (
	"database/sql"
	"testing"
	"time"
)

// TestListRunsRunnerFilter covers the v0.47.19 addition: GET /runs?runnerId=<uuid>
// returns only the runs that runner executed, and every run response now carries
// its real runnerId (previously hard-coded to nil in runToMap).
func TestListRunsRunnerFilter(t *testing.T) {
	ts, db := newTestServer(t)
	seedRunnerWithRuns(t, db)
	client := devLoginClient(t, ts)

	type run struct {
		TraceID  string  `json:"traceId"`
		RunnerID *string `json:"runnerId"`
	}
	type env struct {
		TotalItems int   `json:"totalItems"`
		Items      []run `json:"items"`
	}

	t.Run("filter returns only runner A's runs", func(t *testing.T) {
		var e env
		getJSON(t, client, ts.URL+"/api/v1/runs?runnerId=runner-A", &e)
		if e.TotalItems != 2 {
			t.Fatalf("totalItems = %d, want 2", e.TotalItems)
		}
		for _, r := range e.Items {
			if r.RunnerID == nil || *r.RunnerID != "runner-A" {
				t.Errorf("run %s runnerId = %v, want runner-A", r.TraceID, r.RunnerID)
			}
		}
	})

	t.Run("filter returns only runner B's runs", func(t *testing.T) {
		var e env
		getJSON(t, client, ts.URL+"/api/v1/runs?runnerId=runner-B", &e)
		if e.TotalItems != 1 {
			t.Fatalf("totalItems = %d, want 1", e.TotalItems)
		}
		if r := e.Items[0]; r.RunnerID == nil || *r.RunnerID != "runner-B" {
			t.Errorf("runnerId = %v, want runner-B", r.RunnerID)
		}
	})

	t.Run("unfiltered list carries real runnerId (not nil)", func(t *testing.T) {
		var e env
		getJSON(t, client, ts.URL+"/api/v1/runs", &e)
		var withRunner, ssh int
		for _, r := range e.Items {
			if r.RunnerID != nil {
				withRunner++
			} else {
				ssh++
			}
		}
		if withRunner != 3 {
			t.Errorf("runs with a runnerId = %d, want 3", withRunner)
		}
		if ssh != 1 { // the SSH run has a NULL runner_id
			t.Errorf("runs without a runnerId = %d, want 1", ssh)
		}
	})
}

// seedRunnerWithRuns inserts two runners and four runs: two for runner A, one for
// runner B, and one in-app SSH run with a NULL runner_id.
func seedRunnerWithRuns(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range []string{"runner-A", "runner-B"} {
		if _, err := db.Exec(`
			INSERT INTO runners(id, name, status, registered_at, created_at)
			VALUES (?, ?, 'online', ?, ?)`, id, id, now, now); err != nil {
			t.Fatalf("seed runner %s: %v", id, err)
		}
	}
	rows := []struct {
		id, runner string
	}{
		{"run-a1", "runner-A"},
		{"run-a2", "runner-A"},
		{"run-b1", "runner-B"},
		{"run-ssh", ""}, // NULL runner_id
	}
	for _, r := range rows {
		var runnerID any
		if r.runner != "" {
			runnerID = r.runner
		}
		if _, err := db.Exec(`
			INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, runner_id, created_at)
			VALUES (?, ?, 'bash', 'success', 'test', 'manual', 'runner', ?, ?)`,
			r.id, "job-"+r.id, runnerID, now); err != nil {
			t.Fatalf("seed run %s: %v", r.id, err)
		}
	}
}
