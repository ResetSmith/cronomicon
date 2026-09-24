package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestRunnerTestEndpoint(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLogin(t, ts.URL)

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	recentSeenStr := now.Add(-10 * time.Second).Format(time.RFC3339)
	oldSeenStr := now.Add(-24 * time.Hour).Format(time.RFC3339)

	// Seed one online runner and one offline runner
	if _, err := pool.Exec(`
		INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, last_seen_at, registered_at, created_at)
		VALUES('r-online', 'online-runner', 'online', 'Linux', '["bash"]', 0, 5, '1.0', ?, ?, ?)`,
		recentSeenStr, nowStr, nowStr); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(`
		INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, last_seen_at, registered_at, created_at)
		VALUES('r-offline', 'offline-runner', 'online', 'Linux', '["bash"]', 0, 5, '1.0', ?, ?, ?)`,
		oldSeenStr, nowStr, nowStr); err != nil {
		t.Fatal(err)
	}

	// 1. Missing CSRF token => 403
	{
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runners/r-online/test", nil)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("POST without CSRF = %d, want 403", r.StatusCode)
		}
	}

	// 2. Online runner => 200, status: verified
	{
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runners/r-online/test", nil)
		req.Header.Set("X-CSRF-Token", csrf)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("POST online runner test = %d, want 200", r.StatusCode)
		}
		var resp map[string]any
		if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp["status"] != "verified" {
			t.Errorf("status = %q, want verified", resp["status"])
		}
	}

	// 3. Offline runner => 200, status: conn_error
	{
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runners/r-offline/test", nil)
		req.Header.Set("X-CSRF-Token", csrf)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("POST offline runner test = %d, want 200", r.StatusCode)
		}
		var resp map[string]any
		if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp["status"] != "conn_error" {
			t.Errorf("status = %q, want conn_error", resp["status"])
		}
	}

	// 4. Non-existent runner => 404
	{
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runners/r-ghost/test", nil)
		req.Header.Set("X-CSRF-Token", csrf)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("POST unknown runner = %d, want 404", r.StatusCode)
		}
	}

	// Verify that audit log entries were recorded for the connection tests
	var count int
	err := pool.QueryRow(`
		SELECT COUNT(*) FROM change_log 
		WHERE category = 'Runners' AND action = 'tested'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Errorf("expected at least 2 change log entries for tested runners, got %d", count)
	}
}
