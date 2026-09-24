package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// AR — the deferred-trigger surface: runAt validation, the pending 202 shape,
// visibility on /schedules/upcoming, and the cancel endpoint.

// futureInstant returns a canonical runAt offset from NOW.
//
// These tests used to hardcode 2026-08-05T17:00:00Z as "the future" and
// 2030-01-01 as "beyond the horizon". Both are time bombs, and the first one went
// off: at 17:00Z on 2026-08-05 the deferred trigger began returning 422 "runAt must
// be in the future" and two tests failed for reasons that had nothing to do with
// any change. `parseRunAt` compares against time.Now() and caps at 366 days, so any
// literal instant is only ever valid until the wall clock passes it.
//
// The format matches parseRunAt's canonical output exactly — t.UTC().Format(RFC3339),
// truncated to seconds — so a response echoing the value back compares equal.
func futureInstant(d time.Duration) string {
	return time.Now().Add(d).UTC().Format(time.RFC3339)
}

func TestDeferredRunLifecycle(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (name, source, run_type, concurrency_policy, synced_at)
		VALUES ('deferrable', 'git', 'bash', 'Allow', 't')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	var jobID int64
	_ = pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name='deferrable'`).Scan(&jobID)

	client, csrf := devLoginWithCSRF(t, ts)
	post := func(url string, body any) (*http.Response, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", url, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	runURL := ts.URL + "/api/v1/jobs/" + itoa(jobID) + "/run"

	// ── runAt validation matrix ─────────────────────────────────────────────────
	// Both boundary cases are expressed RELATIVE to now: a fixed "far future"
	// literal silently becomes a "past instant" case once the clock passes it, and
	// the test would still pass — asserting 422 for entirely the wrong reason.
	for _, tc := range []struct {
		name  string
		runAt string
		want  int
	}{
		{"past instant", futureInstant(-24 * time.Hour), http.StatusUnprocessableEntity},
		{"garbage", "next tuesday", http.StatusUnprocessableEntity},
		{"beyond horizon", futureInstant(400 * 24 * time.Hour), http.StatusUnprocessableEntity},
	} {
		if resp, _ := post(runURL, map[string]any{"runAt": tc.runAt}); resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}

	// ── Defer: 202 with a PendingRun, no runs row, envelope frozen ──────────────
	// Two hours out: comfortably in the future, and inside the 7d window the
	// /schedules/upcoming assertion below queries.
	runAt := futureInstant(2 * time.Hour)
	resp, out := post(runURL, map[string]any{
		"runAt": runAt,
		"env":   map[string]string{"MODE": "full"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deferred trigger: status %d (%v)", resp.StatusCode, out)
	}
	if out["pending"] != true || out["runAt"] != runAt {
		t.Fatalf("unexpected pending body: %v", out)
	}
	pendingID, _ := out["id"].(string)
	if pendingID == "" {
		t.Fatal("no pending id returned")
	}
	var runs int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("a deferred trigger created %d immediate runs", runs)
	}
	var params string
	if err := pool.QueryRowContext(ctx,
		`SELECT params_json FROM pending_runs WHERE id = ?`, pendingID).Scan(&params); err != nil {
		t.Fatalf("pending row missing: %v", err)
	}
	// EnvJSON/OverrideJSON are JSON-encoded strings inside the params object, so
	// their quotes arrive escaped — match on the bare tokens.
	for _, want := range []string{"MODE", "full", "scheduledFor", "scheduledBy"} {
		if !bytes.Contains([]byte(params), []byte(want)) {
			t.Errorf("frozen params missing %s: %s", want, params)
		}
	}

	// ── Visible on the upcoming projection, badged ad-hoc ───────────────────────
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/schedules/upcoming?window=7d", nil)
	uresp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	var upcoming struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(uresp.Body).Decode(&upcoming)
	uresp.Body.Close()
	found := false
	for _, it := range upcoming.Items {
		if it["adHoc"] == true && it["pendingId"] == pendingID {
			found = true
			if it["ownerName"] != "deferrable" || it["scheduledBy"] == "" {
				t.Errorf("ad-hoc upcoming item malformed: %v", it)
			}
		}
	}
	if !found {
		t.Fatal("deferred run not visible on /schedules/upcoming")
	}

	// ── Job row carries pendingRunAt ────────────────────────────────────────────
	jreq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(jobID), nil)
	jresp, err := client.Do(jreq)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	var job map[string]any
	_ = json.NewDecoder(jresp.Body).Decode(&job)
	jresp.Body.Close()
	if job["pendingRunAt"] != runAt {
		t.Errorf("job pendingRunAt = %v, want %v", job["pendingRunAt"], runAt)
	}

	// ── Cancel: 204, row gone, second cancel 404 ────────────────────────────────
	del := func() int {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/pending-runs/"+pendingID, nil)
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := del(); got != http.StatusNoContent {
		t.Fatalf("cancel: status %d", got)
	}
	if got := del(); got != http.StatusNotFound {
		t.Errorf("second cancel: status %d, want 404", got)
	}
	var left int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_runs`).Scan(&left)
	if left != 0 {
		t.Errorf("cancelled row still present")
	}
}

// FX2-D3 — the adHoc label is PROVENANCE, not gate-state. An operator's
// deferral whose job is then paused (or binned) acquires a hold stamp on
// gate_kind; the first version derived adHoc from that stamp being empty, so
// the operator's own run flipped to "a gated schedule fire" and the reader was
// sent hunting for a cron entry that does not exist. A held ad-hoc row carries
// BOTH adHoc:true and waitingOn — the truthful state.
func TestHeldDeferralKeepsItsAdHocLabel(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (name, source, run_type, concurrency_policy, synced_at)
		VALUES ('deferrable', 'git', 'bash', 'Allow', 't')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)

	// An operator's deferral, then a hold stamp landing on it (as promotion's
	// holdForGate would when the job is paused before run_at).
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO pending_runs (id, kind, name, source, run_at, scheduled_by, created_at, status, gate_kind)
		VALUES ('pr-held', 'job', 'deferrable', 'git', ?, 'op@example.com', ?, 'pending', 'gate:pause')`,
		futureInstant(-time.Hour), futureInstant(-2*time.Hour)); err != nil {
		t.Fatalf("seed held deferral: %v", err)
	}
	// A queue-parked cron fire for contrast: genuinely NOT ad-hoc.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO pending_runs (id, kind, name, source, run_at, scheduled_by, created_at, status, gate_kind)
		VALUES ('pr-queued', 'job', 'deferrable', 'git', ?, 'scheduler', ?, 'pending', 'concurrency')`,
		futureInstant(-time.Hour), futureInstant(-2*time.Hour)); err != nil {
		t.Fatalf("seed queued row: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/schedules/upcoming?window=7d", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	var upcoming struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&upcoming)
	resp.Body.Close()

	byID := map[string]map[string]any{}
	for _, it := range upcoming.Items {
		if id, _ := it["pendingId"].(string); id != "" {
			byID[id] = it
		}
	}
	held, ok := byID["pr-held"]
	if !ok {
		t.Fatal("the held deferral is not on the upcoming projection at all")
	}
	if held["adHoc"] != true {
		t.Errorf("held deferral adHoc = %v, want true — a hold stamp says why the row is "+
			"WAITING, not who parked it; flipping the label disowns the operator's own run", held["adHoc"])
	}
	if held["waitingOn"] != "pause" {
		t.Errorf("held deferral waitingOn = %v, want pause — both facts are true and both "+
			"belong on the row", held["waitingOn"])
	}
	queued, ok := byID["pr-queued"]
	if !ok {
		t.Fatal("the queued fire is not on the upcoming projection")
	}
	if queued["adHoc"] != false {
		t.Errorf("queue-parked fire adHoc = %v, want false — that label is what tells the "+
			"reader nobody hand-scheduled it", queued["adHoc"])
	}
}

// A deferred WORKFLOW trigger parks a pending row instead of firing.
func TestDeferredWorkflowTrigger(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO workflows (name, source, steps, synced_at) VALUES ('wf-later', 'git', '[]', 't')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	var wfID int64
	_ = pool.QueryRowContext(ctx, `SELECT rowid FROM workflows WHERE name='wf-later'`).Scan(&wfID)

	client, csrf := devLoginWithCSRF(t, ts)
	b, _ := json.Marshal(map[string]any{"runAt": futureInstant(2 * time.Hour)})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/workflows/"+itoa(wfID)+"/trigger", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusAccepted || out["pending"] != true {
		t.Fatalf("deferred workflow trigger: status %d body %v", resp.StatusCode, out)
	}
	var kind string
	if err := pool.QueryRowContext(ctx, `SELECT kind FROM pending_runs WHERE name='wf-later'`).Scan(&kind); err != nil || kind != "workflow" {
		t.Fatalf("pending workflow row: kind=%q err=%v", kind, err)
	}
	var wfRuns int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs`).Scan(&wfRuns)
	if wfRuns != 0 {
		t.Errorf("deferred workflow trigger started %d immediate runs", wfRuns)
	}
}
