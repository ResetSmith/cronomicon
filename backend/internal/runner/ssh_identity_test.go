package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// CA-3b (the ssh-user plan) — the per-run "connect as" identity on the
// runner path: the frozen ssh_credential is an IMPLICIT key binding riding the
// D8 delivery channel, the manifest targets carry the derived reference + user
// override, a local-inventory runner refuses the run, and a credential-carrying
// run is claimable only by an allow_secret_injection runner.

// seedIdentityRun inserts a bash job (no declared bindings), a scope host, and a
// claimed runner run frozen with the given identity. Returns the trace id.
func seedIdentityRun(t *testing.T, svc *Service, runnerID, sshUser, sshCred string) string {
	t.Helper()
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('idjob','amadeus','bash','echo hi','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	seedScopeHost(t, svc, "prod", "web1", "HOST_KEY_NAME")
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, runner_id, executor, triggered_by, trigger_kind, ssh_user, ssh_credential, started_at, created_at)
		VALUES(?, 'idjob', 'amadeus', 'bash', 'prod', 'running', ?, 'runner', 'ops@x', 'manual', ?, ?, ?, ?)`,
		traceID, runnerID, nullIfEmpty(sshUser), nullIfEmpty(sshCred), now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return traceID
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestManifestPerRunCredential — the frozen credential is delivered as key
// material through the D8 channel WITHOUT any declared binding, every target's
// AuthKeyEnvVar becomes the derived CRONOMICON_KEY_<label> reference (which the
// agent resolves to the delivered file), the user override lands on the target,
// and no target ever carries key bytes.
func TestManifestPerRunCredential(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-id1", "amt_run_id1"
	insertRunner(t, svc, runnerID, "id1", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=?, allow_secret_injection=1 WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("flag runner: %v", err)
	}

	const keyMaterial = "PEM-PER-RUN-KEY-MATERIAL-ABC"
	seedKeyCredential(t, svc, "prod-key", keyMaterial)
	traceID := seedIdentityRun(t, svc, runnerID, "deploy", "prod-key")

	m := getManifest(t, svc, as, traceID, tok)

	if len(m.Keys) != 1 || m.Keys[0].Name != "prod-key" || m.Keys[0].Material != keyMaterial {
		t.Fatalf("per-run credential not delivered via D8: %+v", m.Keys)
	}
	if len(m.Targets) != 1 {
		t.Fatalf("expected 1 target, got %+v", m.Targets)
	}
	tg := m.Targets[0]
	if tg.User != "deploy" {
		t.Errorf("target user = %q, want the per-run override 'deploy'", tg.User)
	}
	if tg.AuthKeyEnvVar != "CRONOMICON_KEY_prod-key" {
		t.Errorf("target AuthKeyEnvVar = %q, want the derived CRONOMICON_KEY_prod-key reference", tg.AuthKeyEnvVar)
	}
	// D1 — targets are references only, never bytes.
	for _, x := range m.Targets {
		if x.AuthKeyEnvVar == keyMaterial {
			t.Errorf("key material leaked into a target: %+v", x)
		}
	}
	// M5 — a credential-carrying run arms fail-closed redaction like any key binding.
	var injects bool
	_ = svc.db.QueryRow(`SELECT injects_secret FROM runs WHERE id=?`, traceID).Scan(&injects)
	if !injects {
		t.Errorf("credential-carrying run did not set injects_secret at dispatch")
	}
}

// TestManifestIdentityLocalInventoryRefused — a local-inventory runner resolves
// users/keys from its own inventory, so an identity-override run must 409, not
// silently run as the wrong identity.
func TestManifestIdentityLocalInventoryRefused(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-id2", "amt_run_id2"
	insertRunner(t, svc, runnerID, "id2", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET inventory='local', protocol_version=?, allow_secret_injection=1 WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set local inventory: %v", err)
	}

	traceID := seedIdentityRun(t, svc, runnerID, "deploy", "")

	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("local-inventory manifest with identity override = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// TestClaimGatePerRunCredential — the secret-injection claim fence covers the
// per-run credential column: a non-flagged runner leaves the run queued for a
// flagged one, exactly like a declared binding.
func TestClaimGatePerRunCredential(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	ctx := context.Background()

	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('cjob','amadeus','bash','echo hi','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, status, executor, triggered_by, trigger_kind, ssh_credential, created_at)
		VALUES(?, 'cjob', 'amadeus', 'bash', 'queued', 'runner', 'ops@x', 'manual', 'prod-key', ?)`,
		traceID, now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	insertRunner(t, svc, "r-plain", "plain", "online", []string{"bash"})
	insertRunner(t, svc, "r-flagged", "flagged", "online", []string{"bash"})

	// Non-flagged runner: the fence must leave the credential-carrying run queued.
	got, err := svc.claimRun(ctx, "r-plain", []string{"bash"}, false)
	if err != nil {
		t.Fatalf("claim (plain): %v", err)
	}
	if got != nil {
		t.Fatalf("non-flagged runner claimed a credential-carrying run: %+v", got)
	}

	// Flagged v6 runner: claims it.
	got, err = svc.claimRun(ctx, "r-flagged", []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claim (flagged): %v", err)
	}
	if got == nil || got.TraceID != traceID {
		t.Fatalf("flagged runner should claim the run, got %+v", got)
	}
}

// ── RP-8 · ansible identity carriage (run-parity.md Phase 2) ─────────────────
//
// An ansible run cannot take its identity through ManifestTarget.User the way an
// ssh-family run does: ansible-playbook does the dialing, and the agent's
// localCommand never reads Target.User. So the override travels in explicit
// manifest fields the agent turns into connection extra-vars — and the targets
// are deliberately left untouched, because overlaying AuthKeyEnvVar there would
// make the agent auto-wire --private-key instead, a lower-precedence channel the
// inventory beats per host (the silent half-application RP-Q1 rejected).

// seedAnsibleIdentityRun inserts an ansible job + inventory-bearing scope and a
// claimed runner run frozen with the given identity. Returns the trace id.
func seedAnsibleIdentityRun(t *testing.T, svc *Service, runnerID, sshUser, sshCred string) string {
	t.Helper()
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('ansjob','amadeus','ansible','site.yml','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	seedScopeHost(t, svc, "prod", "web1", "HOST_KEY_NAME")
	// The scope needs a managed inventory or the ansible run 409s on no_inventory
	// before it ever reaches the identity carriage.
	if _, err := svc.db.Exec(
		`UPDATE scopes SET raw_inventory=?, inventory_format='ini' WHERE name='prod'`,
		"[web]\nweb1 ansible_host=10.0.0.1\n"); err != nil {
		t.Fatalf("attach inventory: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, runner_id, executor, triggered_by, trigger_kind, ssh_user, ssh_credential, started_at, created_at)
		VALUES(?, 'ansjob', 'amadeus', 'ansible', 'prod', 'running', ?, 'runner', 'ops@x', 'manual', ?, ?, ?, ?)`,
		traceID, runnerID, nullIfEmpty(sshUser), nullIfEmpty(sshCred), now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return traceID
}

// TestManifestAnsibleIdentityFields — the override rides sshUser/sshKeyRef, the
// key material is still delivered through the D8 channel, and the TARGETS are
// left alone so nothing auto-wires a lower-precedence --private-key.
func TestManifestAnsibleIdentityFields(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-rp8", "amt_run_rp8"
	insertRunner(t, svc, runnerID, "rp8", "online", []string{"ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=?, allow_secret_injection=1 WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("flag runner: %v", err)
	}

	const keyMaterial = "PEM-ANSIBLE-OVERRIDE-KEY"
	seedKeyCredential(t, svc, "prod-key", keyMaterial)
	traceID := seedAnsibleIdentityRun(t, svc, runnerID, "deploy", "prod-key")

	m := getManifest(t, svc, as, traceID, tok)

	if m.SSHUser != "deploy" {
		t.Errorf("manifest sshUser = %q, want deploy", m.SSHUser)
	}
	if m.SSHKeyRef != "CRONOMICON_KEY_prod-key" {
		t.Errorf("manifest sshKeyRef = %q, want the derived CRONOMICON_KEY_prod-key reference", m.SSHKeyRef)
	}
	// Names only (D1) — the reference, never the bytes.
	if m.SSHKeyRef == keyMaterial || m.SSHUser == keyMaterial {
		t.Error("key material leaked into the identity fields")
	}
	// The material still travels the reviewed D8 channel, via the implicit
	// KindKey binding — that is what the agent resolves sshKeyRef against.
	if len(m.Keys) != 1 || m.Keys[0].Name != "prod-key" || m.Keys[0].Material != keyMaterial {
		t.Fatalf("ansible override key not delivered via D8: %+v", m.Keys)
	}
	// The targets keep their INVENTORY identity: the override is applied by
	// ansible from the extra-vars, not by rewriting the host list.
	for _, tg := range m.Targets {
		if tg.AuthKeyEnvVar == "CRONOMICON_KEY_prod-key" {
			t.Errorf("ansible run overlaid the target key ref (%+v) — that would auto-wire --private-key, which the inventory outranks", tg)
		}
	}
}

// A plain ansible run on an old agent is untouched by the new gate — it fires
// only when the run actually carries an override.
func TestManifestAnsibleNoIdentityOldAgentStillRuns(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-rp8plain", "amt_run_rp8plain"
	insertRunner(t, svc, runnerID, "rp8plain", "online", []string{"ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol: %v", err)
	}
	traceID := seedAnsibleIdentityRun(t, svc, runnerID, "", "")

	m := getManifest(t, svc, as, traceID, tok)
	if m.SSHUser != "" || m.SSHKeyRef != "" {
		t.Errorf("plain run carried identity fields: (%q,%q)", m.SSHUser, m.SSHKeyRef)
	}
}

// TestManifestSSHFamilyIdentityUnchanged — the ssh-family channel must NOT move
// to the new fields: the agent dials those targets itself, and an agent of any
// protocol version honors the target overlay.
func TestManifestSSHFamilyIdentityUnchanged(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-rp8ssh", "amt_run_rp8ssh"
	insertRunner(t, svc, runnerID, "rp8ssh", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=?, allow_secret_injection=1 WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("flag runner: %v", err)
	}
	seedKeyCredential(t, svc, "prod-key", "PEM-Y")
	traceID := seedIdentityRun(t, svc, runnerID, "deploy", "prod-key")

	m := getManifest(t, svc, as, traceID, tok)
	if m.SSHUser != "" || m.SSHKeyRef != "" {
		t.Errorf("ssh-family run set the ansible-only fields: (%q,%q)", m.SSHUser, m.SSHKeyRef)
	}
	if len(m.Targets) != 1 || m.Targets[0].User != "deploy" || m.Targets[0].AuthKeyEnvVar != "CRONOMICON_KEY_prod-key" {
		t.Errorf("ssh-family target overlay regressed: %+v", m.Targets)
	}
}

// ── Phase 3 · advanced ansible options carriage + v8 gate (RP-17) ───────────

// seedAnsibleOptsRun is seedAnsibleIdentityRun with an override envelope instead
// of an identity, so the option path can be exercised on its own.
func seedAnsibleOptsRun(t *testing.T, svc *Service, runnerID, overrideJSON string) string {
	t.Helper()
	if _, err := svc.db.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('optjob','amadeus','ansible','site.yml','Allow',?)`, now()); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	seedScopeHost(t, svc, "prod", "web1", "HOST_KEY_NAME")
	if _, err := svc.db.Exec(
		`UPDATE scopes SET raw_inventory=?, inventory_format='ini' WHERE name='prod'`,
		"[web]\nweb1 ansible_host=10.0.0.1\n"); err != nil {
		t.Fatalf("attach inventory: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, run_type, scope, status, runner_id, executor, triggered_by, trigger_kind, override_json, started_at, created_at)
		VALUES(?, 'optjob', 'amadeus', 'ansible', 'prod', 'running', ?, 'runner', 'ops@x', 'manual', ?, ?, ?)`,
		traceID, runnerID, nullIfEmpty(overrideJSON), now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return traceID
}

func TestManifestAnsibleOptionsCarried(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-p3", "amt_run_p3"
	insertRunner(t, svc, runnerID, "p3", "online", []string{"ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol: %v", err)
	}
	traceID := seedAnsibleOptsRun(t, svc, runnerID,
		`{"ansibleCheck":true,"ansibleTags":["certs"],"ansibleVerbosity":2,"ansibleExtraVars":{"env_name":"staging"}}`)

	m := getManifest(t, svc, as, traceID, tok)
	if m.AnsibleOptions == nil {
		t.Fatal("manifest carried no ansibleOptions")
	}
	if !m.AnsibleOptions.Check || m.AnsibleOptions.Verbosity != 2 {
		t.Errorf("options mis-carried: %+v", m.AnsibleOptions)
	}
	if len(m.AnsibleOptions.Tags) != 1 || m.AnsibleOptions.Tags[0] != "certs" {
		t.Errorf("tags mis-carried: %+v", m.AnsibleOptions.Tags)
	}
	if m.AnsibleOptions.ExtraVars["env_name"] != "staging" {
		t.Errorf("extra-vars mis-carried: %+v", m.AnsibleOptions.ExtraVars)
	}
}

// A run carrying no advanced option must stay claimable by ANY agent — the gate
// keys on "did the operator set something", not on the run type.
func TestManifestNoAnsibleOptionsOldAgentStillRuns(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-p3plain", "amt_run_p3plain"
	insertRunner(t, svc, runnerID, "p3plain", "online", []string{"ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol: %v", err)
	}
	traceID := seedAnsibleOptsRun(t, svc, runnerID, `{"hosts":["web1"]}`)

	m := getManifest(t, svc, as, traceID, tok)
	if m.AnsibleOptions != nil {
		t.Errorf("plain run grew an options block: %+v", m.AnsibleOptions)
	}
}
