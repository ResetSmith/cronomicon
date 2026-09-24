package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// AW-14 — activation windows across the write surfaces: the round-trip through
// the first-class catalog, propagation into every referencing definition, the
// ref-expansion copy, and the validation matrix.

func TestScheduleWindowRoundTripAndPropagation(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// ── Create with a window ────────────────────────────────────────────────────
	resp := do(http.MethodPost, ts.URL+"/api/v1/schedule-defs", map[string]any{
		"name":    "deferred",
		"cron":    "0 17 * * 3",
		"startAt": "2026-08-05T00:00:00Z",
		"endAt":   "2026-12-31T00:00:00Z",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with window: status %d", resp.StatusCode)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created["startAt"] != "2026-08-05T00:00:00Z" {
		t.Fatalf("startAt did not round-trip: %v", created["startAt"])
	}
	if created["endAt"] != "2026-12-31T00:00:00Z" {
		t.Fatalf("endAt did not round-trip: %v", created["endAt"])
	}

	// A non-UTC offset must normalize to canonical UTC on the way in.
	resp = do(http.MethodPost, ts.URL+"/api/v1/schedule-defs", map[string]any{
		"name":    "offset-tz",
		"cron":    "0 9 * * *",
		"startAt": "2026-08-05T17:00:00-04:00",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with offset tz: status %d", resp.StatusCode)
	}
	var offsetRow map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&offsetRow)
	resp.Body.Close()
	if offsetRow["startAt"] != "2026-08-05T21:00:00Z" {
		t.Fatalf("offset not normalized to UTC: %v", offsetRow["startAt"])
	}

	// ── A job binding the schedule by ref copies the window down (AW-5) ─────────
	resp = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name":         "nightly-backup",
		"scriptRef":    "backup-db",
		"scope":        "",
		"scheduleRefs": []string{"deferred"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create job binding schedule: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	var gotStart, gotEnd, gotRef string
	if err := pool.QueryRowContext(ctx, `
		SELECT COALESCE(start_at,''), COALESCE(end_at,''), COALESCE(source_ref,'')
		FROM definition_schedules
		WHERE owner_kind='job' AND owner_name='nightly-backup' AND name='deferred'`).
		Scan(&gotStart, &gotEnd, &gotRef); err != nil {
		t.Fatalf("read expanded entry: %v", err)
	}
	if gotStart != "2026-08-05T00:00:00Z" || gotEnd != "2026-12-31T00:00:00Z" {
		t.Fatalf("ref expansion did not copy the window: start=%q end=%q", gotStart, gotEnd)
	}
	if gotRef != "deferred" {
		t.Fatalf("source_ref = %q, want the schedule name", gotRef)
	}

	// ── Editing the schedule propagates the new window (AW-4) ───────────────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/schedule-defs/deferred", map[string]any{
		"cron":    "0 17 * * 3",
		"startAt": "2026-09-01T00:00:00Z",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit schedule: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	if err := pool.QueryRowContext(ctx, `
		SELECT COALESCE(start_at,''), COALESCE(end_at,'')
		FROM definition_schedules
		WHERE owner_kind='job' AND owner_name='nightly-backup' AND name='deferred'`).
		Scan(&gotStart, &gotEnd); err != nil {
		t.Fatalf("re-read expanded entry: %v", err)
	}
	if gotStart != "2026-09-01T00:00:00Z" {
		t.Fatalf("edited startAt did not propagate: %q", gotStart)
	}
	// Clearing endAt on edit must clear it downstream too, not leave a stale bound.
	if gotEnd != "" {
		t.Fatalf("cleared endAt did not propagate: %q", gotEnd)
	}
}

// An inline (job-local) schedule carries its own window, independent of any
// first-class catalog row.
func TestJobComposeInlineScheduleWindow(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)

	body, _ := json.Marshal(map[string]any{
		"name":      "inline-window-job",
		"scriptRef": "backup-db",
		"scope":     "",
		"schedules": []map[string]any{
			{"name": "deferred", "cron": "0 17 * * 3", "startAt": "2026-08-05T00:00:00Z"},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create job with inline window: status %d", resp.StatusCode)
	}

	var start, sourceRef string
	if err := pool.QueryRowContext(ctx, `
		SELECT COALESCE(start_at,''), COALESCE(source_ref,'')
		FROM definition_schedules WHERE owner_name='inline-window-job' AND name='deferred'`).
		Scan(&start, &sourceRef); err != nil {
		t.Fatalf("read inline entry: %v", err)
	}
	if start != "2026-08-05T00:00:00Z" {
		t.Fatalf("inline window not persisted: %q", start)
	}
	if sourceRef != "" {
		t.Fatalf("inline entry must have a NULL source_ref, got %q", sourceRef)
	}
}

// The validation matrix: malformed bounds and an inverted window are 422s, and
// omitting the fields entirely stays valid (the pre-window behavior).
func TestScheduleWindowValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	post := func(body map[string]any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/schedule-defs", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"no window", map[string]any{"name": "plain", "cron": "0 7 * * *"}, http.StatusCreated},
		{"blank bounds", map[string]any{"name": "blank", "cron": "0 7 * * *", "startAt": "", "endAt": ""}, http.StatusCreated},
		{"bad startAt", map[string]any{"name": "bad1", "cron": "0 7 * * *", "startAt": "next tuesday"}, http.StatusUnprocessableEntity},
		{"bad endAt", map[string]any{"name": "bad2", "cron": "0 7 * * *", "endAt": "2026-13-45"}, http.StatusUnprocessableEntity},
		{"start after end", map[string]any{
			"name": "bad3", "cron": "0 7 * * *",
			"startAt": "2026-09-01T00:00:00Z", "endAt": "2026-08-01T00:00:00Z",
		}, http.StatusUnprocessableEntity},
		{"start equals end", map[string]any{
			"name": "bad4", "cron": "0 7 * * *",
			"startAt": "2026-09-01T00:00:00Z", "endAt": "2026-09-01T00:00:00Z",
		}, http.StatusUnprocessableEntity},
		// AW-18 — the unanchored-interval descriptor stays rejected at the API too.
		{"@every rejected", map[string]any{"name": "interval", "cron": "@every 168h"}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		if got := post(tc.body); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Phase 2 — the interval and one-shot modes across the write surfaces.
func TestScheduleIntervalModes(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, csrf := devLoginWithCSRF(t, ts)

	post := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/schedule-defs", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// An interval schedule round-trips and reports its derived mode.
	status, created := post(map[string]any{
		"name":     "every-ten-days",
		"interval": "10d",
		"startAt":  "2026-08-05T17:00:00Z",
	})
	if status != http.StatusCreated {
		t.Fatalf("create interval schedule: status %d (%v)", status, created)
	}
	if created["interval"] != "10d" {
		t.Errorf("interval did not round-trip: %v", created["interval"])
	}
	if created["mode"] != "interval" {
		t.Errorf("mode = %v, want interval", created["mode"])
	}

	// A one-shot: an anchor with neither cron nor interval.
	status, once := post(map[string]any{"name": "just-once", "startAt": "2026-08-05T17:00:00Z"})
	if status != http.StatusCreated {
		t.Fatalf("create one-shot: status %d (%v)", status, once)
	}
	if once["mode"] != "once" {
		t.Errorf("mode = %v, want once", once["mode"])
	}

	// The mode contract: exclusivity, the required anchor, and a sane floor.
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"cron and interval together", map[string]any{
			"name": "both", "cron": "0 7 * * *", "interval": "7d", "startAt": "2026-08-05T00:00:00Z",
		}, http.StatusUnprocessableEntity},
		{"interval without anchor", map[string]any{"name": "unanchored", "interval": "7d"}, http.StatusUnprocessableEntity},
		{"sub-minute interval", map[string]any{
			"name": "toofast", "interval": "30s", "startAt": "2026-08-05T00:00:00Z",
		}, http.StatusUnprocessableEntity},
		{"garbage interval", map[string]any{
			"name": "garbage", "interval": "every other tuesday", "startAt": "2026-08-05T00:00:00Z",
		}, http.StatusUnprocessableEntity},
		{"nothing at all", map[string]any{"name": "empty"}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		if got, _ := post(tc.body); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}

	// A job binding the interval schedule copies the whole rule down, so the
	// runtime row is self-contained (the scheduler never joins back).
	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"name": "interval-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"every-ten-days"},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create job binding interval schedule: status %d", resp.StatusCode)
	}
	var gotInterval, gotStart, gotCron string
	if err := pool.QueryRowContext(ctx, `
		SELECT COALESCE(interval,''), COALESCE(start_at,''), cron
		FROM definition_schedules WHERE owner_name='interval-job' AND name='every-ten-days'`).
		Scan(&gotInterval, &gotStart, &gotCron); err != nil {
		t.Fatalf("read expanded entry: %v", err)
	}
	if gotInterval != "10d" || gotStart != "2026-08-05T17:00:00Z" {
		t.Fatalf("ref expansion lost the interval rule: interval=%q start=%q", gotInterval, gotStart)
	}
	if gotCron != "" {
		t.Errorf("an interval entry must carry no cron, got %q", gotCron)
	}
}
