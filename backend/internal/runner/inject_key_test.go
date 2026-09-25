package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// seedKeyCredential inserts a stored-source ssh_credentials row (sealed material)
// so a key binding resolves to material.
func seedKeyCredential(t *testing.T, svc *Service, label, material string) {
	t.Helper()
	sealed, err := secrets.NewSealer(svc.cfg).Seal([]byte(material))
	if err != nil {
		t.Fatalf("seal key %q: %v", label, err)
	}
	if _, err := svc.db.Exec(
		`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version,
		                              created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, ?, 'stored', ?, ?, ?, ?, 'tester', ?, 'tester', ?)`,
		db.NewID(), label, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion, now(), now()); err != nil {
		t.Fatalf("insert ssh credential %q: %v", label, err)
	}
}

// TestManifestDeliversKeyMaterial (D8): a job that binds an SSH key ships the
// resolved key MATERIAL in the manifest Keys block to a flagged v6 runner, and the
// delivery is audited (a change_log "Secrets"/"injected" row names the key).
func TestManifestDeliversKeyMaterial(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-key", "crn_run_key"
	insertRunner(t, svc, runnerID, "key", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=?, allow_secret_injection=1 WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("flag runner: %v", err)
	}

	const keyMaterial = "PEM-DEPLOY-KEY-MATERIAL-XYZ"
	seedKeyCredential(t, svc, "deploy_key", keyMaterial)
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('jk','amadeus','bash','echo hi','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','jk','key','deploy_key',?)`, now()); err != nil {
		t.Fatalf("seed key binding: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, runner_id, executor, triggered_by, trigger_kind, started_at, created_at)
		VALUES(?, 'jk', 'amadeus', 'bash', 'prod', 'running', ?, 'runner', 'ops@x', 'manual', ?, ?)`,
		traceID, runnerID, now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	m := getManifest(t, svc, as, traceID, tok)

	if len(m.Keys) != 1 {
		t.Fatalf("expected 1 delivered key, got %d: %+v", len(m.Keys), m.Keys)
	}
	k := m.Keys[0]
	if k.Name != "deploy_key" || k.Reference != "CRONOMICON_KEY_deploy_key" || k.Material != keyMaterial {
		t.Errorf("delivered key wrong: %+v", k)
	}
	// The material must NOT leak into the plaintext Env or the Secrets value block.
	for kk, vv := range m.Env {
		if vv == keyMaterial {
			t.Errorf("key material leaked into Env[%q]", kk)
		}
	}
	for kk, vv := range m.Secrets {
		if vv == keyMaterial {
			t.Errorf("key material leaked into Secrets[%q]", kk)
		}
	}

	// D8: the delivered key is audited (change_log names it; never the material).
	var count int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target=?`, traceID).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 injection audit row, got %d", count)
	}
	var details string
	_ = svc.db.QueryRow(`SELECT details FROM change_log WHERE category='Secrets' AND action='injected' AND target=?`, traceID).Scan(&details)
	if !strings.Contains(details, "deploy_key") || strings.Contains(details, keyMaterial) {
		t.Errorf("audit details wrong (must name key, never material): %s", details)
	}

	// D8: a key-bearing run is armed for fail-closed redaction (M5 dispatch flag).
	var injects bool
	_ = svc.db.QueryRow(`SELECT injects_secret FROM runs WHERE id=?`, traceID).Scan(&injects)
	if !injects {
		t.Errorf("key-bearing run did not set injects_secret at dispatch")
	}
}

// TestIngestMasksDeliveredKeyMaterial (D8 lockstep): a delivered key's material
// echoed into a run's output is MASKED at ingest — the ingest redaction path now
// resolves keys into the dictionary, so relaxing the pre-Resolve key filter cannot
// leak key bytes to the stored log.
func TestIngestMasksDeliveredKeyMaterial(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-keymask", "crn_run_keymask"
	insertRunner(t, svc, runnerID, "keymask", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	const keyMaterial = "PEM-SECRET-KEY-TO-MASK-9f3a"
	seedKeyCredential(t, svc, "deploy_key", keyMaterial)
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('jk','amadeus','bash','echo hi','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','jk','key','deploy_key',?)`, now()); err != nil {
		t.Fatalf("seed key binding: %v", err)
	}
	traceID := db.NewTraceID()
	// injects_secret=1: a real dispatch of this key-bearing run sets the M5 flag.
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, runner_id, executor, triggered_by, trigger_kind, injects_secret, started_at, created_at)
		VALUES(?, 'jk', 'amadeus', 'bash', 'prod', 'running', ?, 'runner', 'ops@x', 'manual', 1, ?, ?)`,
		traceID, runnerID, now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	body := "cat key: " + keyMaterial + "\n" +
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
	if strings.Contains(string(data), keyMaterial) {
		t.Errorf("delivered key material leaked into persisted log:\n%s", data)
	}
	if !strings.Contains(string(data), "[REDACTED]") {
		t.Errorf("expected redaction marker in persisted log:\n%s", data)
	}
}
