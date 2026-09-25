package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestIngestRefusesOutputLeakingSecret (H2/DEC-2): a run that captures an injected
// secret value into an ::cronomicon-output:: marker is failed CLOSED at ingest — the
// output is not persisted (nothing propagates downstream), the run is marked
// failed with reason output_secret_leak, and the secret is still masked in the log.
func TestIngestRefusesOutputLeakingSecret(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-leak", "crn_run_leak"
	insertRunner(t, svc, runnerID, "leak", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, secretVal, _ := seedInjectionRun(t, svc, runnerID, 6, true)

	// The natural leak idiom: echo the injected secret into an output marker.
	body := "::cronomicon-output name=TOKEN::" + secretVal + "\n" +
		`{"exitCode":0,"durationMs":10,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)

	// The chunk is accepted (204) — refusing is a run-level decision, not a retry.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ingest: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	// The run is failed closed with the distinct reason.
	var status, reason string
	_ = svc.db.QueryRow(`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE id=?`, traceID).Scan(&status, &reason)
	if status != "failure" {
		t.Errorf("run status = %q, want failure (fail-closed on output leak)", status)
	}
	if reason != "output_secret_leak" {
		t.Errorf("run reason = %q, want output_secret_leak", reason)
	}

	// The output value must NOT have been persisted — nothing carries it forward.
	var outputs string
	_ = svc.db.QueryRow(`SELECT COALESCE(outputs_json,'') FROM runs WHERE id=?`, traceID).Scan(&outputs)
	if outputs != "" {
		t.Errorf("leaking output persisted to outputs_json: %q", outputs)
	}

	// The secret is still masked in the log, and a refusal notice names the output.
	data, err := os.ReadFile(mustLogPath(t, svc, traceID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(data), secretVal) {
		t.Errorf("secret value leaked into persisted log:\n%s", data)
	}
	if !strings.Contains(string(data), `output "TOKEN" would leak an injected secret`) {
		t.Errorf("expected refusal notice naming the output:\n%s", data)
	}
}

// TestIngestCapturesNonLeakingOutput (H2): the refuse guard is narrow — an output
// that does NOT carry an injected secret value is captured and the run succeeds as
// before, so the injection idiom does not break ordinary inter-step outputs.
func TestIngestCapturesNonLeakingOutput(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-ok", "crn_run_ok"
	insertRunner(t, svc, runnerID, "ok", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, _, _ := seedInjectionRun(t, svc, runnerID, 6, true)

	body := "::cronomicon-output name=DB_HOST::pg-prod-01\n" +
		`{"exitCode":0,"durationMs":10,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ingest: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	var status, outputs string
	_ = svc.db.QueryRow(`SELECT status, COALESCE(outputs_json,'') FROM runs WHERE id=?`, traceID).Scan(&status, &outputs)
	if status != "success" {
		t.Errorf("run status = %q, want success", status)
	}
	if !strings.Contains(outputs, "pg-prod-01") {
		t.Errorf("non-leaking output not captured: %q", outputs)
	}
}
