package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

const testKEK = "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="

// enableInjection turns on dispatch-time injection + a KEK on the service's shared
// config (the resolver, secrets service, and sealer all hold the same pointer).
func enableInjection(svc *Service) {
	svc.cfg.SecretsInjectionEnabled = true
	svc.cfg.SecretKEKEnv = testKEK
}

// seedInjectionRun seeds an amadeus job 'j1' + a stored secret + variable (scope
// 'prod') + reference bindings for the job + a claimed runner run, and returns the
// trace id. protocolVersion + allowInject configure the assigned runner.
func seedInjectionRun(t *testing.T, svc *Service, runnerID string, protocolVersion int, allowInject bool) (traceID, secretVal, varVal string) {
	t.Helper()
	ctx := context.Background()
	secretVal, varVal = "PROD-DB-PASSWORD-XYZ", "us-east-2"

	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=?, allow_secret_injection=? WHERE id=?`,
		protocolVersion, allowInject, runnerID); err != nil {
		t.Fatalf("configure runner: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	sc, err := secrets.New(svc.db, svc.cfg, svc.log).Create(ctx,
		secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new("prod"), Value: secretVal}, "tester")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	_ = sc
	if _, err := svc.db.Exec(`INSERT INTO env_vars(id, key, value, scope, created_by, created_at, last_modified_by, last_modified_at)
		VALUES('v1','REGION',?,'prod','t',?,'t',?)`, varVal, now(), now()); err != nil {
		t.Fatalf("seed var: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j1','secret','DB_PASS',?),('job','amadeus','j1','var','REGION',?)`, now(), now()); err != nil {
		t.Fatalf("seed bindings: %v", err)
	}
	traceID = db.NewTraceID()
	// injects_secret=1: this fixture seeds a SECRET binding, so a real dispatch of
	// this run would set the M5 dispatch flag when the manifest ships the value. The
	// ingest fail-closed guard reads this flag (not live bindings), so the fixture
	// must reflect a dispatched secret-bearing run.
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, runner_id, executor, triggered_by, trigger_kind, injects_secret, started_at, created_at)
		VALUES(?, 'j1', 'amadeus', 'bash', 'prod', 'running', ?, 'runner', 'ops@x', 'manual', 1, ?, ?)`,
		traceID, runnerID, now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return traceID, secretVal, varVal
}

//go:fix inline

func callManifest(t *testing.T, svc *Service, as *auth.Service, traceID, token string) *httptest.ResponseRecorder {
	t.Helper()
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestManifestInjectsReferences (P1.4): a binding-bearing run assigned to a v6,
// injection-flagged runner ships resolved Secrets + the CRONOMICON_RUN_* context.
func TestManifestInjectsReferences(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-inj", "crn_run_inj"
	insertRunner(t, svc, runnerID, "inj", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	traceID, secretVal, varVal := seedInjectionRun(t, svc, runnerID, 6, true)
	m := getManifest(t, svc, as, traceID, tok)

	if m.Secrets["CRONOMICON_SECRET_DB_PASS"] != secretVal {
		t.Errorf("secret not injected: %q", m.Secrets["CRONOMICON_SECRET_DB_PASS"])
	}
	if m.Secrets["CRONOMICON_VAR_REGION"] != varVal {
		t.Errorf("variable not injected: %q", m.Secrets["CRONOMICON_VAR_REGION"])
	}
	// Run context lands in the plaintext Env, NOT the Secrets block.
	if m.Env["CRONOMICON_RUN_ID"] != traceID || m.Env["CRONOMICON_RUN_EXECUTOR"] != "runner" ||
		m.Env["CRONOMICON_RUN_JOB"] != "j1" || m.Env["CRONOMICON_RUN_SCOPE"] != "prod" ||
		m.Env["CRONOMICON_RUN_TRIGGERED_BY"] != "ops@x" {
		t.Errorf("run context missing/wrong in Env: %+v", m.Env)
	}
	if _, leaked := m.Env["CRONOMICON_SECRET_DB_PASS"]; leaked {
		t.Errorf("secret value leaked into plaintext Env: %+v", m.Env)
	}
}

// TestManifestInjectsOverrideReferences (V2-11, stored-reference additions): a
// reference the operator attached per-run via the override envelope is resolved
// into the manifest exactly like a declared binding — same Secrets block, same
// dedupe against the declared set.
func TestManifestInjectsOverrideReferences(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-ovr", "crn_run_ovr"
	insertRunner(t, svc, runnerID, "ovr", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	traceID, secretVal, varVal := seedInjectionRun(t, svc, runnerID, 6, true)
	// An EXTRA variable the job does not declare, attached per-run — plus a
	// duplicate of the declared secret, which must dedupe rather than double up.
	if _, err := svc.db.Exec(`INSERT INTO env_vars(id, key, value, scope, created_by, created_at, last_modified_by, last_modified_at)
		VALUES('v-extra','EXTRA','extra-value','prod','t',?,'t',?)`, now(), now()); err != nil {
		t.Fatalf("seed extra var: %v", err)
	}
	if _, err := svc.db.Exec(`UPDATE runs SET override_json='{"references":[{"kind":"var","name":"EXTRA"},{"kind":"secret","name":"DB_PASS"}]}' WHERE id=?`, traceID); err != nil {
		t.Fatalf("set override: %v", err)
	}

	m := getManifest(t, svc, as, traceID, tok)
	if m.Secrets["CRONOMICON_VAR_EXTRA"] != "extra-value" {
		t.Errorf("per-run added variable not injected: %q", m.Secrets["CRONOMICON_VAR_EXTRA"])
	}
	if m.Secrets["CRONOMICON_SECRET_DB_PASS"] != secretVal {
		t.Errorf("declared secret lost after per-run addition: %q", m.Secrets["CRONOMICON_SECRET_DB_PASS"])
	}
	if m.Secrets["CRONOMICON_VAR_REGION"] != varVal {
		t.Errorf("declared variable lost after per-run addition: %q", m.Secrets["CRONOMICON_VAR_REGION"])
	}
}

// TestManifestSecretInjectionRunnerNotFlagged (P1.4): defense-in-depth — a v6
// runner that is NOT flagged for injection never receives resolved secrets.
func TestManifestSecretInjectionRunnerNotFlagged(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-noinj", "crn_run_noinj"
	insertRunner(t, svc, runnerID, "noinj", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	traceID, _, _ := seedInjectionRun(t, svc, runnerID, 6, false) // v6 but NOT flagged
	rec := callManifest(t, svc, as, traceID, tok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret_injection_not_allowed") {
		t.Errorf("expected secret_injection_not_allowed, got: %s", rec.Body.String())
	}
}

// TestClaimRunSecretInjectionGate (P1.4): a queued run whose job declares
// reference bindings is claimed only by a runner passed allowSecretInjection=true.
func TestClaimRunSecretInjectionGate(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.SecretsInjectionEnabled = true // fence is active only when injection is on
	ctx := context.Background()
	runnerID := "runner-gate"
	insertRunner(t, svc, runnerID, "gate", "online", []string{"bash"})

	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES(?, 'jb', 'amadeus', 'bash', 'prod', 'queued', 'test', 'manual', 'runner', ?)`,
		traceID, now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','jb','secret','DB_PASS',?)`, now()); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	// A non-injection runner must NOT claim the binding-bearing run.
	got, err := svc.claimRun(ctx, runnerID, []string{"bash"}, false)
	if err != nil {
		t.Fatalf("claimRun(false): %v", err)
	}
	if got != nil {
		t.Fatalf("non-injection runner claimed a binding-bearing run: %+v", got)
	}
	// The same runner, flagged, claims it.
	got, err = svc.claimRun(ctx, runnerID, []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun(flagged v6): %v", err)
	}
	if got == nil || got.TraceID != traceID {
		t.Fatalf("flagged v6 runner failed to claim binding-bearing run: %+v", got)
	}
}

// TestClaimRunGateDisarmedByKillSwitch (P1.4 review fix): with the global
// injection kill-switch OFF, the claim fence is disarmed — a NON-injection runner
// may claim a binding-bearing run (which will simply inject nothing), so declaring
// a binding cannot strand runs when the feature is disabled.
func TestClaimRunGateDisarmedByKillSwitch(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.SecretsInjectionEnabled = false // kill-switch OFF
	ctx := context.Background()
	runnerID := "runner-ks"
	insertRunner(t, svc, runnerID, "ks", "online", []string{"bash"})

	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES(?, 'jb', 'amadeus', 'bash', 'prod', 'queued', 'test', 'manual', 'runner', ?)`,
		traceID, now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','jb','secret','DB_PASS',?)`, now()); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	// allowSecretInjection=false, but the kill-switch disarms the fence.
	got, err := svc.claimRun(ctx, runnerID, []string{"bash"}, false)
	if err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	if got == nil || got.TraceID != traceID {
		t.Fatalf("kill-switch off did not disarm the fence: %+v", got)
	}
}
