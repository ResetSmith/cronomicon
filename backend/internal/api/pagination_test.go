package api_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// TestPageSizeClamp is the PP-H5 regression: pageSize is clamped to the OpenAPI
// maximum (200) so an over-large value cannot drive an unbounded LIMIT that
// materializes an entire table into one response. page is deliberately NOT
// capped (a large OFFSET returns an empty body cheaply).
func TestPageSizeClamp(t *testing.T) {
	ts, db := newTestServer(t)
	seedRuns(t, db, 205)
	client := devLoginClient(t, ts)

	type pageEnvelope struct {
		Page       int   `json:"page"`
		PageSize   int   `json:"pageSize"`
		TotalItems int   `json:"totalItems"`
		Items      []any `json:"items"`
	}

	cases := []struct {
		name         string
		query        string
		wantPageSize int
		wantItems    int
	}{
		{"over-max clamps to 200", "pageSize=100000000", 200, 200},
		{"normal page size honored", "pageSize=50", 50, 50},
		{"zero falls back to default", "pageSize=0", 50, 50},
		{"negative falls back to default", "pageSize=-1", 50, 50},
		{"huge page returns empty (page NOT capped)", "page=100000000&pageSize=50", 50, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env pageEnvelope
			getJSON(t, client, ts.URL+"/api/v1/runs?"+tc.query, &env)
			if env.PageSize != tc.wantPageSize {
				t.Errorf("pageSize = %d, want %d", env.PageSize, tc.wantPageSize)
			}
			if len(env.Items) != tc.wantItems {
				t.Errorf("len(items) = %d, want %d", len(env.Items), tc.wantItems)
			}
			if env.TotalItems != 205 {
				t.Errorf("totalItems = %d, want 205", env.TotalItems)
			}
		})
	}

	// The clamp must hold on every paginated list family, not just /runs.
	for _, path := range []string{"/api/v1/jobs", "/api/v1/scripts", "/api/v1/schedule-defs", "/api/v1/activity"} {
		t.Run("clamp on "+path, func(t *testing.T) {
			var env pageEnvelope
			getJSON(t, client, ts.URL+path+"?pageSize=100000000", &env)
			if env.PageSize != 200 {
				t.Errorf("%s pageSize = %d, want 200", path, env.PageSize)
			}
		})
	}
}

// seedRuns inserts n minimal runs so a paginated list has more than one page.
func seedRuns(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range n {
		if _, err := db.Exec(`
			INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, created_at)
			VALUES (?, ?, 'bash', 'success', 'test', 'manual', 'ssh', ?)`,
			fmt.Sprintf("run-%04d", i), fmt.Sprintf("job-%04d", i), now); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
	}
}
