package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestIngestPartialChunkLiveTail (v12 live tailing): a chunk marked
// X-Log-Partial: 1 is a mid-run flush — it must be persisted and advance the
// raw offset WITHOUT finalizing the run (an unmarked envelope-less chunk
// finalizes log_stream_lost, the pre-v12 contract). The terminal chunk then
// arrives unmarked with the trailing envelope and finalizes normally.
func TestIngestPartialChunkLiveTail(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-live", "crn_run_live"
	insertRunner(t, svc, runnerID, "live", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	traceID := "run-live-1"
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, executor, created_at, started_at)
		VALUES (?, 'j', 'bash', 'prod', 'running', ?, 'test', 'manual', 'runner', ?, ?)`,
		traceID, runnerID, ts, ts); err != nil {
		t.Fatal(err)
	}

	post := func(body, resumeOffset string, partial bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Resume-Offset", resumeOffset)
		if partial {
			req.Header.Set("X-Log-Partial", "1")
		}
		req.SetPathValue("traceId", traceID)
		rec := httptest.NewRecorder()
		as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
		return rec
	}
	status := func() string {
		var s string
		if err := svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, traceID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// First mid-run flush: persisted, offset advanced, run STILL running.
	chunk1 := "hello\nworld\n"
	if rec := post(chunk1, "0", true); rec.Code != http.StatusNoContent {
		t.Fatalf("partial chunk 1: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	if s := status(); s != "running" {
		t.Fatalf("after partial chunk run status = %q, want running", s)
	}
	data, err := os.ReadFile(mustLogPath(t, svc, traceID))
	if err != nil {
		t.Fatalf("read log after partial chunk: %v", err)
	}
	if got := string(data); got != "hello\nworld\n" {
		t.Errorf("persisted log after partial chunk = %q", got)
	}
	var off int64
	_ = svc.db.QueryRow(`SELECT log_raw_offset FROM runs WHERE id=?`, traceID).Scan(&off)
	if off != int64(len(chunk1)) {
		t.Errorf("log_raw_offset = %d, want %d", off, len(chunk1))
	}

	// Second mid-run flush resumes at the committed offset and appends.
	chunk2 := "more\n"
	if rec := post(chunk2, "12", true); rec.Code != http.StatusNoContent {
		t.Fatalf("partial chunk 2: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	if s := status(); s != "running" {
		t.Fatalf("after partial chunk 2 run status = %q, want running", s)
	}

	// Terminal flush: unmarked, envelope-terminated — finalizes the run.
	envelope := `{"exitCode":0,"durationMs":1500,"endedAt":"2026-08-18T00:00:00Z"}` + "\n"
	if rec := post("done\n"+envelope, "17", false); rec.Code != http.StatusNoContent {
		t.Fatalf("terminal chunk: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	if s := status(); s != "success" {
		t.Errorf("after terminal chunk run status = %q, want success", s)
	}
	data, _ = os.ReadFile(mustLogPath(t, svc, traceID))
	if !strings.Contains(string(data), "hello\nworld\nmore\ndone\n") {
		t.Errorf("final log missing streamed lines:\n%s", data)
	}
}
