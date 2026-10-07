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

// TestIngestCapturesAnOutputBehindTheAgentsHostPrefix: a shell job an agent
// runs over SSH reaches the server as "[host] line" — the agent prefixes every
// remote line, for one target as for several (agent/ssh.go). The marker was
// looked for at the start of the raw line, so on that path it never matched and
// inter-job outputs were silently empty. The leak guard must reach the same
// values: an output captured from behind the prefix is still refused when it
// carries an injected secret.
func TestIngestCapturesAnOutputBehindTheAgentsHostPrefix(t *testing.T) {
	// One service per run: seedInjectionRun writes a secret under a fixed key.
	var svc *Service
	ingest := func(runnerID, tok, body string) (traceID, secret string) {
		t.Helper()
		svc = newTestService(t)
		enableInjection(svc)
		as := authSvc(t, svc)
		insertRunner(t, svc, runnerID, runnerID, "online", []string{"bash"})
		bindRunnerToken(t, svc, tok, runnerID)
		traceID, secret, _ = seedInjectionRun(t, svc, runnerID, 6, true)
		body = strings.ReplaceAll(body, "{secret}", secret) +
			`{"exitCode":0,"durationMs":10,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.SetPathValue("traceId", traceID)
		rec := httptest.NewRecorder()
		as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("ingest: got %d, want 204; body: %s", rec.Code, rec.Body.String())
		}
		return traceID, secret
	}

	traceID, _ := ingest("runner-prefix", "crn_run_prefix",
		"[web-01] starting\n"+
			"[web-01] ::cronomicon-output name=DB_HOST::pg-prod-01\n"+
			"[web-02] ::cronomicon-output name=REGION::us-east\n"+
			"[web-01] not a marker ::cronomicon-output name=NOPE::x\n")
	var status, outputs string
	_ = svc.db.QueryRow(`SELECT status, COALESCE(outputs_json,'') FROM runs WHERE id=?`, traceID).Scan(&status, &outputs)
	if status != "success" {
		t.Errorf("run status = %q, want success", status)
	}
	for _, want := range []string{`"DB_HOST":"pg-prod-01"`, `"REGION":"us-east"`} {
		if !strings.Contains(outputs, want) {
			t.Errorf("outputs_json = %q, want it to hold %s (the marker sits behind the agent's [host] prefix)", outputs, want)
		}
	}
	if strings.Contains(outputs, "NOPE") {
		t.Errorf("a marker in the middle of a line was captured: %q", outputs)
	}

	leakID, _ := ingest("runner-prefix-leak", "crn_run_prefix_leak",
		"[web-01] ::cronomicon-output name=TOKEN::{secret}\n")
	var reason string
	_ = svc.db.QueryRow(`SELECT status, COALESCE(queued_reason,''), COALESCE(outputs_json,'') FROM runs WHERE id=?`, leakID).Scan(&status, &reason, &outputs)
	if status != "failure" || reason != "output_secret_leak" || outputs != "" {
		t.Errorf("a secret captured from behind the prefix: status %q reason %q outputs %q, want it refused like any other", status, reason, outputs)
	}
}

// On a run the agent does NOT prefix (ansible, terraform), a leading "[…] " is
// the tool's or the job's own text. Nothing is read past it: data a playbook
// echoes after a bracketed tag must not become an output. A marker that starts
// the line still does.
func TestIngestReadsNoMarkerBehindABracketOnAnUnprefixedRun(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-ansible", "crn_run_ansible"
	insertRunner(t, svc, runnerID, "ansible", "online", []string{"bash", "ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, _, _ := seedInjectionRun(t, svc, runnerID, 6, true)
	if _, err := svc.db.Exec(`UPDATE runs SET run_type = 'ansible' WHERE id = ?`, traceID); err != nil {
		t.Fatal(err)
	}

	body := "[INFO] ::cronomicon-output name=FORGED::1\n" +
		"::cronomicon-output name=REAL::2\n" +
		`{"exitCode":0,"durationMs":10,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ingest: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	var outputs string
	_ = svc.db.QueryRow(`SELECT COALESCE(outputs_json,'') FROM runs WHERE id=?`, traceID).Scan(&outputs)
	if strings.Contains(outputs, "FORGED") {
		t.Errorf("a marker behind a bracketed tag was captured on an ansible run: %q", outputs)
	}
	if !strings.Contains(outputs, `"REAL":"2"`) {
		t.Errorf("a marker that starts the line was not captured: %q", outputs)
	}
}
