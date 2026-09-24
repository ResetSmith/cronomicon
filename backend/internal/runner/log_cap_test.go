package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestIngestCapsRunLogSize (SU-9): the log-ingest endpoint is exempt from the
// global 2 MiB body cap, so a per-run ceiling (AMADEUS_MAX_RUN_LOG_BYTES) is the
// only bound on how much a runner can write. Once a run's persisted log reaches
// the ceiling, further ingest is refused with 413 and nothing more is appended —
// and a subsequent chunk short-circuits to 413 without writing.
func TestIngestCapsRunLogSize(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.MaxRunLogBytes = 200 // tiny ceiling for the test
	as := authSvc(t, svc)
	runnerID, tok := "runner-cap", "amt_run_cap"
	insertRunner(t, svc, runnerID, "cap", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	// A running run owned by this runner.
	traceID := "run-cap-1"
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, executor, created_at, started_at)
		VALUES (?, 'j', 'bash', 'prod', 'running', ?, 'test', 'manual', 'runner', ?, ?)`,
		traceID, runnerID, ts, ts); err != nil {
		t.Fatal(err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.SetPathValue("traceId", traceID)
		rec := httptest.NewRecorder()
		as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
		return rec
	}

	// A single line well over the 200-byte ceiling.
	if rec := post(strings.Repeat("x", 500) + "\n"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap ingest: got %d, want 413; body: %s", rec.Code, rec.Body.String())
	}

	// The oversized line was NOT persisted — only a short truncation notice is.
	data, err := os.ReadFile(mustLogPath(t, svc, traceID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if int64(len(data)) > int64(svc.cfg.MaxRunLogBytes) {
		t.Errorf("persisted log %d bytes exceeds ceiling %d:\n%s", len(data), svc.cfg.MaxRunLogBytes, data)
	}
	if strings.Contains(string(data), strings.Repeat("x", 500)) {
		t.Errorf("oversized line was persisted despite the cap")
	}
	if !strings.Contains(string(data), "run log size limit reached") {
		t.Errorf("expected a truncation notice in the log:\n%s", data)
	}

	// The raw offset is pinned at the ceiling so a follow-up chunk short-circuits.
	var off int64
	_ = svc.db.QueryRow(`SELECT log_raw_offset FROM runs WHERE id=?`, traceID).Scan(&off)
	if off != int64(svc.cfg.MaxRunLogBytes) {
		t.Errorf("log_raw_offset = %d, want %d (pinned at ceiling)", off, svc.cfg.MaxRunLogBytes)
	}

	// A subsequent chunk is refused at the pre-check without appending anything.
	before, _ := os.ReadFile(mustLogPath(t, svc, traceID))
	if rec := post("more output\n"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("follow-up ingest: got %d, want 413", rec.Code)
	}
	after, _ := os.ReadFile(mustLogPath(t, svc, traceID))
	if len(after) != len(before) {
		t.Errorf("follow-up chunk appended %d bytes after the cap", len(after)-len(before))
	}
}
