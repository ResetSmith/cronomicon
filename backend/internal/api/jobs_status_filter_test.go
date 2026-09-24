package api_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// TestJobsStatusFilterPagination is the CC.2 regression: the jobs-list status=
// filter must apply BEFORE COUNT(*) and LIMIT/OFFSET. The old shape derived
// status per row AFTER pagination and filtered in Go, so a filtered request
// returned short pages and the unfiltered totals.
//
// Seeded statuses interleave in name order (status assigned by index mod 10),
// so a post-pagination filter could not accidentally return full pages.
func TestJobsStatusFilterPagination(t *testing.T) {
	ts, db := newTestServer(t)
	seedStatusJobs(t, db, 150)
	client := devLoginClient(t, ts)

	type jobItem struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	type pageEnvelope struct {
		Page       int       `json:"page"`
		PageSize   int       `json:"pageSize"`
		TotalItems int       `json:"totalItems"`
		TotalPages int       `json:"totalPages"`
		Items      []jobItem `json:"items"`
	}

	// 150 jobs: i%10 0-6 → success (105), 7 → danger (15), 8 → paused (15,
	// alternating enabled=0 and paused_jobs rows), 9 → idle (15).
	cases := []struct {
		status     string
		wantTotal  int
		wantPages  int
		pageChecks map[int]int // page → expected item count
	}{
		{"success", 105, 3, map[int]int{1: 50, 2: 50, 3: 5}},
		{"danger", 15, 1, map[int]int{1: 15}},
		{"paused", 15, 1, map[int]int{1: 15}},
		{"idle", 15, 1, map[int]int{1: 15}},
	}
	for _, tc := range cases {
		for page, wantItems := range tc.pageChecks {
			t.Run(fmt.Sprintf("status=%s page=%d", tc.status, page), func(t *testing.T) {
				var env pageEnvelope
				getJSON(t, client, fmt.Sprintf("%s/api/v1/jobs?status=%s&pageSize=50&page=%d", ts.URL, tc.status, page), &env)
				if env.TotalItems != tc.wantTotal {
					t.Errorf("totalItems = %d, want %d", env.TotalItems, tc.wantTotal)
				}
				if env.TotalPages != tc.wantPages {
					t.Errorf("totalPages = %d, want %d", env.TotalPages, tc.wantPages)
				}
				if len(env.Items) != wantItems {
					t.Errorf("len(items) = %d, want %d (short page ⇒ filter ran after pagination)", len(env.Items), wantItems)
				}
				for _, it := range env.Items {
					if it.Status != tc.status {
						t.Errorf("item %q status = %q, want %q", it.Name, it.Status, tc.status)
					}
				}
			})
		}
	}

	// Unfiltered list still counts everything (CC.1: the one surviving COUNT
	// must be correct).
	t.Run("unfiltered total", func(t *testing.T) {
		var env pageEnvelope
		getJSON(t, client, ts.URL+"/api/v1/jobs?pageSize=50", &env)
		if env.TotalItems != 150 {
			t.Errorf("totalItems = %d, want 150", env.TotalItems)
		}
		if env.TotalPages != 3 {
			t.Errorf("totalPages = %d, want 3", env.TotalPages)
		}
		if len(env.Items) != 50 {
			t.Errorf("len(items) = %d, want 50", len(env.Items))
		}
	})
}

// seedStatusJobs inserts n jobs whose derived display status cycles by i%10 so
// statuses interleave in name (=pagination) order:
//
//	0-6 → success (latest run success; i==0 also gets an older failure run to
//	      pin the "latest run wins" semantics)
//	7   → danger  (latest run failure)
//	8   → paused  (even i: enabled=0; odd i: paused_jobs row)
//	9   → idle    (no runs)
func seedStatusJobs(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	now := time.Now().UTC()
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for i := range n {
		name := fmt.Sprintf("j-%03d", i)
		enabled := 1
		if i%10 == 8 && i%2 == 0 {
			enabled = 0
		}
		if _, err := db.Exec(`
			INSERT INTO jobs(name, source, run_type, command, enabled, content_hash, synced_at)
			VALUES (?, 'git', 'bash', 'echo hi', ?, 'h', ?)`, name, enabled, stamp(0)); err != nil {
			t.Fatalf("seed job %s: %v", name, err)
		}
		runStatus := ""
		switch {
		case i%10 <= 6:
			runStatus = "success"
		case i%10 == 7:
			runStatus = "failure"
		case i%10 == 8 && i%2 == 1:
			if _, err := db.Exec(`
				INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at)
				VALUES ('git', 'job', ?, 'test', ?)`, name, stamp(0)); err != nil {
				t.Fatalf("seed paused_jobs %s: %v", name, err)
			}
		}
		if runStatus != "" {
			if _, err := db.Exec(`
				INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, executor, created_at)
				VALUES (?, ?, 'git', 'bash', ?, 'test', 'manual', 'ssh', ?)`,
				fmt.Sprintf("r-%03d", i), name, runStatus, stamp(0)); err != nil {
				t.Fatalf("seed run for %s: %v", name, err)
			}
		}
		if i == 0 {
			// Older failure behind the latest success: status must stay "success".
			if _, err := db.Exec(`
				INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, executor, created_at)
				VALUES (?, ?, 'git', 'bash', 'failure', 'test', 'manual', 'ssh', ?)`,
				"r-old-000", name, stamp(-time.Hour)); err != nil {
				t.Fatalf("seed old run: %v", err)
			}
		}
	}
}
