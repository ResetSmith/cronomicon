package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestRunInjectionRedactionTokens (P1.5): the ingest redaction helper surfaces the
// injected SECRET value (so the log sink can mask it) and reports injectsSecret,
// while the log-safe VARIABLE value is deliberately excluded from the dictionary.
func TestRunInjectionRedactionTokens(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	runnerID := "runner-red"
	insertRunner(t, svc, runnerID, "red", "online", []string{"bash"})
	_, secretVal, varVal := seedInjectionRun(t, svc, runnerID, 6, true)

	redact, injectsSecret, err := svc.runInjectionRedaction(context.Background(), "", "j1", "amadeus", "", "prod")
	if err != nil {
		t.Fatalf("runInjectionRedaction: %v", err)
	}
	if !injectsSecret {
		t.Errorf("expected injectsSecret=true for a secret-bound run")
	}
	if !slices.Contains(redact, secretVal) {
		t.Errorf("injected secret value missing from redaction tokens: %v", redact)
	}
	if slices.Contains(redact, varVal) {
		t.Errorf("log-safe variable value must NOT enter the redaction dictionary (D7): %v", redact)
	}
}

// TestRunInjectionRedactionVarOnly (P1.5): a run that binds only a variable does
// NOT report injectsSecret (so a redactor-build failure stays lenient) and adds no
// tokens — the variable value is log-safe.
func TestRunInjectionRedactionVarOnly(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j2','amadeus','bash','echo hi','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO env_vars(id, key, value, scope, created_by, created_at, last_modified_by, last_modified_at)
		VALUES('vv','REGION','us-west-2','prod','t',?,'t',?)`, now(), now()); err != nil {
		t.Fatalf("seed var: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j2','var','REGION',?)`, now()); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	redact, injectsSecret, err := svc.runInjectionRedaction(context.Background(), "", "j2", "amadeus", "", "prod")
	if err != nil {
		t.Fatalf("runInjectionRedaction: %v", err)
	}
	if injectsSecret {
		t.Errorf("variable-only run must not report injectsSecret")
	}
	if len(redact) != 0 {
		t.Errorf("variable value is log-safe; redaction tokens must be empty, got %v", redact)
	}
}

// TestRunInjectionRedactionKillSwitch (P1.5): with the injection kill-switch off the
// helper is inert — no bindings are read and nothing is reported.
func TestRunInjectionRedactionKillSwitch(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc) // sets the KEK so the seed can encrypt; injection on for now
	runnerID := "runner-ks-red"
	insertRunner(t, svc, runnerID, "ksred", "online", []string{"bash"})
	seedInjectionRun(t, svc, runnerID, 6, true)
	// Flip the kill-switch off: the helper must short-circuit before reading bindings.
	svc.cfg.SecretsInjectionEnabled = false

	redact, injectsSecret, err := svc.runInjectionRedaction(context.Background(), "", "j1", "amadeus", "", "prod")
	if err != nil || injectsSecret || len(redact) != 0 {
		t.Errorf("kill-switch off must be inert: redact=%v injectsSecret=%v err=%v", redact, injectsSecret, err)
	}
}

// TestIngestMasksInjectedSecret (P1.5): an injected secret echoed in a run's output
// is masked in the persisted log through the ingest redactor.
func TestIngestMasksInjectedSecret(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-mask", "crn_run_mask"
	insertRunner(t, svc, runnerID, "mask", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, secretVal, _ := seedInjectionRun(t, svc, runnerID, 6, true)

	body := "connecting with " + secretVal + " now\n" +
		`{"exitCode":0,"durationMs":10,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ingest: got %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(mustLogPath(t, svc, traceID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(data), secretVal) {
		t.Errorf("injected secret leaked into persisted log:\n%s", data)
	}
	if !strings.Contains(string(data), "[REDACTED]") {
		t.Errorf("expected redaction marker in persisted log:\n%s", data)
	}
}

// TestIngestFailsClosedOnUnresolvableSecret (P1.5): if a secret-bearing run's value
// becomes unmaskable at ingest (its row deleted after dispatch, or a Vault outage),
// the handler fails closed — it persists NOTHING (no log file, no offset advance, no
// finalize) rather than write raw bytes that might contain the un-masked secret.
func TestIngestFailsClosedOnUnresolvableSecret(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-fc", "crn_run_fc"
	insertRunner(t, svc, runnerID, "fc", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, secretVal, _ := seedInjectionRun(t, svc, runnerID, 6, true)

	// The binding stays (the run injects a secret) but the value is now unresolvable.
	if _, err := svc.db.Exec(`DELETE FROM secrets WHERE key='DB_PASS'`); err != nil {
		t.Fatalf("delete secret: %v", err)
	}

	body := "echo " + secretVal + "\n" +
		`{"exitCode":0,"durationMs":1,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "redactor_unavailable") {
		t.Errorf("expected redactor_unavailable, got: %s", rec.Body.String())
	}
	if _, err := os.Stat(mustLogPath(t, svc, traceID)); !os.IsNotExist(err) {
		t.Errorf("fail-closed must not create the log file (stat err=%v)", err)
	}
	var offset int64
	_ = svc.db.QueryRow(`SELECT COALESCE(log_raw_offset,0) FROM runs WHERE id=?`, traceID).Scan(&offset)
	if offset != 0 {
		t.Errorf("offset advanced despite fail-closed: %d", offset)
	}
	var status string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, traceID).Scan(&status)
	if status != "running" {
		t.Errorf("run finalized despite fail-closed: %q", status)
	}
}

// TestIngestFailsClosedAfterBindingDeletedMidRun (M5): the verified bypass —
// a secret-bearing run whose binding is DELETED mid-run (job pruned, or a
// full-replace PUT that drops it) yields a clean-EMPTY enumeration (no error), so
// the old live-binding check flipped to the lenient path and persisted an
// already-injected value un-redacted. With the decision authoritative to the
// dispatch flag (runs.injects_secret), the run stays fail-closed for life.
func TestIngestFailsClosedAfterBindingDeletedMidRun(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-del", "crn_run_del"
	insertRunner(t, svc, runnerID, "del", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, secretVal, _ := seedInjectionRun(t, svc, runnerID, 6, true) // sets injects_secret=1

	// Delete the SECRET binding mid-run (the secret row itself still exists and
	// resolves fine — so enumeration succeeds cleanly with the secret simply gone
	// from the run's binding set: injectErr==nil, injectRedact empty).
	if _, err := svc.db.Exec(
		`DELETE FROM reference_bindings WHERE owner_name='j1' AND ref_kind='secret' AND ref_name='DB_PASS'`); err != nil {
		t.Fatalf("delete binding: %v", err)
	}

	body := "late echo of " + secretVal + "\n" +
		`{"exitCode":0,"durationMs":1,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (fail-closed after binding deleted); body: %s", rec.Code, rec.Body.String())
	}
	// The value must NOT have been persisted.
	if _, err := os.Stat(mustLogPath(t, svc, traceID)); !os.IsNotExist(err) {
		t.Errorf("fail-closed must not create the log file (stat err=%v)", err)
	}
	var status string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, traceID).Scan(&status)
	if status != "running" {
		t.Errorf("run finalized despite fail-closed: %q", status)
	}
}

// TestIngestFailsClosedWhenBindingsUnenumerable (P1.5 review fix): if the ingest
// handler cannot even ENUMERATE a run's bindings (a transient reference_bindings
// error), it cannot prove the run is secret-free, so it fails closed rather than
// persist raw bytes. This is the branch that protects Phase-2 vault-source values,
// which are absent from the global stored-secret dictionary — a single enumeration
// error must not silently fall through to a lenient redactor.
func TestIngestFailsClosedWhenBindingsUnenumerable(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-enum", "crn_run_enum"
	insertRunner(t, svc, runnerID, "enum", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "some-job", "bash", "prod", runnerID, "")
	// M5: this run injected a secret at dispatch (flag set), and its bindings are now
	// unenumerable — the fail-closed guard must refuse the chunk.
	if _, err := svc.db.Exec(`UPDATE runs SET injects_secret=1 WHERE id=?`, traceID); err != nil {
		t.Fatalf("set injects_secret: %v", err)
	}

	// Force binding enumeration to fail: drop the table the resolver reads.
	if _, err := svc.db.Exec(`DROP TABLE reference_bindings`); err != nil {
		t.Fatalf("drop reference_bindings: %v", err)
	}

	body := "some output\n" +
		`{"exitCode":0,"durationMs":1,"endedAt":"2026-07-20T00:00:00Z"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 on unenumerable bindings; body: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(mustLogPath(t, svc, traceID)); !os.IsNotExist(err) {
		t.Errorf("fail-closed must not create the log file (stat err=%v)", err)
	}
}
