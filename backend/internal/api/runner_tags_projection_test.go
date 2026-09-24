package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// RT-1 — the runner_tags projection (mig. 1070,
// the runner-targeting plan).
//
// Runner tags became dispatch-affecting: claimRun probes the projection, not the
// JSON column. So the two must never diverge — a runner claiming work for a tag
// it no longer carries, or refusing work for one it does, is a routing bug that
// surfaces nowhere near the tag editor that caused it.
//
// TestUpdateRunnerTags already covers the endpoint's contract (CSRF, 422, 404,
// normalization). This file covers only the projection's agreement with the
// column, which is the part the shared writeTagsUpdate helper could not provide.
func TestRunnerTagsProjectionTracksColumn(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	const rid = "019f4dea-e8e6-7587-8a2c-2d9f562f1c8e"
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO runners(id, name, status, registered_at, created_at)
		 VALUES(?, 'r1', 'online', ?, ?)`, rid, now, now); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	put := func(runnerID string, tags []string) int {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"tags": tags})
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/runner-tags/"+runnerID, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	projected := func() []string {
		t.Helper()
		rows, err := pool.Query(`SELECT tag FROM runner_tags WHERE runner_id=? ORDER BY tag`, rid)
		if err != nil {
			t.Fatalf("read projection: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var tag string
			if err := rows.Scan(&tag); err != nil {
				t.Fatal(err)
			}
			out = append(out, tag)
		}
		return out
	}

	// The projection carries the NORMALIZED set, not the raw request — otherwise
	// the claim query would be matching against strings the UI never shows.
	if code := put(rid, []string{" vlan-dmz ", "east", "", "VLAN-DMZ"}); code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", code)
	}
	if got := projected(); len(got) != 2 || got[0] != "east" || got[1] != "vlan-dmz" {
		t.Fatalf("projection = %v, want [east vlan-dmz]", got)
	}

	// Full replace REMOVES what is gone. A projection that only ever accumulated
	// would keep dispatching to a runner after its tag was taken away — the
	// failure mode with the longest fuse, since nothing looks wrong until a run
	// lands somewhere it should not.
	if code := put(rid, []string{"vlan-core"}); code != http.StatusOK {
		t.Fatalf("PUT replace = %d, want 200", code)
	}
	if got := projected(); len(got) != 1 || got[0] != "vlan-core" {
		t.Fatalf("projection after replace = %v, want [vlan-core]", got)
	}

	// Clearing to [] empties the projection entirely.
	if code := put(rid, []string{}); code != http.StatusOK {
		t.Fatalf("PUT clear = %d, want 200", code)
	}
	if got := projected(); len(got) != 0 {
		t.Fatalf("projection after clear = %v, want empty", got)
	}
}

// TestRunnerTagsProjectionUntouchedOn404 — the write is transactional, so a PUT
// at a runner that does not exist must leave no trace. Without the rollback the
// DELETE would still have landed, silently unpinning a live runner whose id
// happened to be mistyped into someone else's request.
func TestRunnerTagsProjectionUntouchedOn404(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	const rid = "019f4dea-e8e6-7587-8a2c-2d9f562f1c8f"
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO runners(id, name, status, tags, registered_at, created_at)
		 VALUES(?, 'r1', 'online', '["vlan-dmz"]', ?, ?)`, rid, now, now); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO runner_tags(runner_id, tag) VALUES(?, 'vlan-dmz')`, rid); err != nil {
		t.Fatalf("seed projection: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	b, _ := json.Marshal(map[string]any{"tags": []string{"whatever"}})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/runner-tags/no-such-runner", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown runner = %d, want 404", resp.StatusCode)
	}

	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runner_tags WHERE runner_id=?`, rid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("projection rows for the untouched runner = %d, want 1", n)
	}
}
