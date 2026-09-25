package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// bindRunnerToken inserts a runner_tokens row bound to runnerID (R1.4) so
// auth.RequireRunner resolves the caller's runner identity from the bearer
// token, exactly as registration wires it.
func bindRunnerToken(t *testing.T, svc *Service, token, runnerID string) {
	t.Helper()
	ts := now()
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_, err := svc.db.Exec(`
		INSERT INTO runner_tokens(token_hash, runner_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		auth.HashToken(token), runnerID, "runner:"+runnerID, ts, expiry)
	if err != nil {
		t.Fatalf("bindRunnerToken: %v", err)
	}
}

// insertClaimedRun inserts a claimed (running, executor=runner) run owned by
// runnerID, for manifest/log ownership tests.
func insertClaimedRun(t *testing.T, svc *Service, traceID, jobName, runType, scope, runnerID, envJSON string) {
	t.Helper()
	ts := now()
	_, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, executor,
		                 env_json, triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, ?, ?, ?, 'running', ?, 'runner', ?, 'test', 'manual', ?, ?)`,
		traceID, jobName, runType, scope, runnerID, nullIf(envJSON), ts, ts)
	if err != nil {
		t.Fatalf("insertRunningRun: %v", err)
	}
}

// insertClaimedRunOverride is insertClaimedRun with an override_json envelope
// (M3 — to exercise the manifest --limit computation from groups/hosts/ansibleLimit).
func insertClaimedRunOverride(t *testing.T, svc *Service, traceID, jobName, runType, scope, runnerID, overrideJSON string) {
	t.Helper()
	ts := now()
	_, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, executor,
		                 override_json, triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, ?, ?, ?, 'running', ?, 'runner', ?, 'test', 'manual', ?, ?)`,
		traceID, jobName, runType, scope, runnerID, nullIf(overrideJSON), ts, ts)
	if err != nil {
		t.Fatalf("insertClaimedRunOverride: %v", err)
	}
}

// insertClaimedRunTargetHost is insertClaimedRun with runs.target_host set (the
// TG-3/TG-4 job pin), to exercise the manifest's PinnedAnsibleLimit folding and
// the metachar-pin 409 backstop.
func insertClaimedRunTargetHost(t *testing.T, svc *Service, traceID, jobName, runType, scope, runnerID, targetHost string) {
	t.Helper()
	ts := now()
	_, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, target_host, status, runner_id, executor,
		                 triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, ?, ?, ?, ?, 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
		traceID, jobName, runType, scope, targetHost, runnerID, ts, ts)
	if err != nil {
		t.Fatalf("insertClaimedRunTargetHost: %v", err)
	}
}

// insertClaimedRunTargetHostOverride is insertClaimedRunTargetHost with an
// ADDITIONAL override_json envelope, to exercise a run that carries both a
// jobs.target_host pin AND a per-run override (e.g. a raw ansibleLimit
// passthrough, TG-4's scoping case for the metachar-pin 409 backstop).
func insertClaimedRunTargetHostOverride(t *testing.T, svc *Service, traceID, jobName, runType, scope, runnerID, targetHost, overrideJSON string) {
	t.Helper()
	ts := now()
	_, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, target_host, override_json, status, runner_id, executor,
		                 triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
		traceID, jobName, runType, scope, targetHost, nullIf(overrideJSON), runnerID, ts, ts)
	if err != nil {
		t.Fatalf("insertClaimedRunTargetHostOverride: %v", err)
	}
}

// TestManifestPinnedLimitMetacharWithAnsibleLimitPassthrough is the complement of
// TestManifestPinnedLimitMetacharRejected: the metachar-pin 409 backstop is
// scoped to the branch where the pin is actually FOLDED into --limit. When the
// run's override_json carries a raw ansibleLimit passthrough, the operator has
// taken manual control of --limit and RunLimit never folds the pin in — so
// there is nothing to widen, and the 409 must NOT fire. The manifest's Limit
// must equal the raw passthrough value verbatim, not the (metachar) pin.
func TestManifestPinnedLimitMetacharWithAnsibleLimitPassthrough(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-badpin-passthrough", "crn_run_badpin_pass"
	insertRunner(t, svc, runnerID, "badpinpass", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)
	seedAnsibleScope(t, svc, "prod", "[web]\nweb1\n")
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n", 0)

	traceID := db.NewTraceID()
	insertClaimedRunTargetHostOverride(t, svc, traceID, "patch", "ansible", "prod", runnerID,
		"web[01:50]", `{"ansibleLimit":"web:&staged:!quarantine"}`)

	m := getManifest(t, svc, as, traceID, tok)
	if m.Limit != "web:&staged:!quarantine" {
		t.Errorf("limit = %q, want the verbatim ansibleLimit passthrough (pin must not be folded in)", m.Limit)
	}
}

// TestManifestPinnedLimit verifies the TG-3 fix: a job pinned to one host
// (runs.target_host) folds into the manifest's ansible --limit in BOTH
// inventory modes, so a pinned ansible run no longer ships no --limit (i.e.
// runs the full inventory) on a manual trigger.
func TestManifestPinnedLimit(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-pin", "crn_run_pin"
	insertRunner(t, svc, runnerID, "pin", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)
	seedAnsibleScope(t, svc, "prod", "[web]\nweb1\nweb2\n")
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n", 0)

	// amadeus-mode pinned ansible run -> Limit == "<host>".
	t1 := db.NewTraceID()
	insertClaimedRunTargetHost(t, svc, t1, "patch", "ansible", "prod", runnerID, "web1")
	m := getManifest(t, svc, as, t1, tok)
	if m.Limit != "web1" {
		t.Errorf("amadeus-mode pinned limit = %q, want web1", m.Limit)
	}

	// local-inventory-mode pinned ansible run ships the SAME limit (names only,
	// no inventory leak — the two executors must not disagree on --limit).
	if _, err := svc.db.Exec(`UPDATE runners SET inventory='local' WHERE id=?`, runnerID); err != nil {
		t.Fatalf("set local: %v", err)
	}
	t2 := db.NewTraceID()
	insertClaimedRunTargetHost(t, svc, t2, "patch", "ansible", "prod", runnerID, "web1")
	m2 := getManifest(t, svc, as, t2, tok)
	if m2.Limit != "web1" {
		t.Errorf("local-mode pinned limit = %q, want web1", m2.Limit)
	}
	if m2.Inventory != nil {
		t.Errorf("local-mode must NOT ship inventory, got %+v", m2.Inventory)
	}
}

// TestManifestPinnedLimitMetacharRejected is the TG-4 backstop: a jobs.target_host
// that fails execspec.ValidName (an ansible pattern metacharacter) would be
// silently DROPPED by AnsibleLimit, leaving the run with no --limit at all — a
// pinned job running the entire inventory. The manifest handler refuses this
// outright (409 invalid_target_host) rather than run wide.
func TestManifestPinnedLimitMetacharRejected(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-badpin", "crn_run_badpin"
	insertRunner(t, svc, runnerID, "badpin", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)
	seedAnsibleScope(t, svc, "prod", "[web]\nweb1\n")
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n", 0)

	traceID := db.NewTraceID()
	insertClaimedRunTargetHost(t, svc, traceID, "patch", "ansible", "prod", runnerID, "web[01:50]")

	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("metachar-pinned ansible run: got %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_target_host") {
		t.Errorf("expected error code invalid_target_host, got %s", rec.Body.String())
	}
}

// TestManifestLimit verifies the M3 --limit computation: structured group/host
// names in amadeus mode, local-mode ships Limit but NO inventory (no-leak), and a
// raw ansibleLimit passthrough takes precedence over the structured limit.
func TestManifestLimit(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-lim", "crn_run_lim"
	insertRunner(t, svc, runnerID, "lim", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)
	seedAnsibleScope(t, svc, "prod", "[web]\nweb1\n")
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n", 0)

	// amadeus ansible run with override groups → Limit = group NAMES + Inventory shipped.
	t1 := db.NewTraceID()
	insertClaimedRunOverride(t, svc, t1, "patch", "ansible", "prod", runnerID, `{"groups":["web","db"]}`)
	m := getManifest(t, svc, as, t1, tok)
	if m.Limit != "web,db" {
		t.Errorf("structured limit = %q, want web,db", m.Limit)
	}
	if m.Inventory == nil {
		t.Errorf("amadeus ansible run should ship inventory")
	}

	// Raw ansibleLimit passthrough takes precedence over the structured limit.
	t2 := db.NewTraceID()
	insertClaimedRunOverride(t, svc, t2, "patch", "ansible", "prod", runnerID, `{"groups":["web"],"ansibleLimit":"web:&staged:!quarantine"}`)
	m2 := getManifest(t, svc, as, t2, tok)
	if m2.Limit != "web:&staged:!quarantine" {
		t.Errorf("raw passthrough limit = %q, want the verbatim ansibleLimit", m2.Limit)
	}

	// Local-mode runner: Limit is still shipped (names only) but Inventory stays nil
	// (no-leak — the host list never crosses the wire).
	if _, err := svc.db.Exec(`UPDATE runners SET inventory='local' WHERE id=?`, runnerID); err != nil {
		t.Fatalf("set local: %v", err)
	}
	t3 := db.NewTraceID()
	insertClaimedRunOverride(t, svc, t3, "patch", "ansible", "prod", runnerID, `{"groups":["web"]}`)
	m3 := getManifest(t, svc, as, t3, tok)
	if m3.Limit != "web" {
		t.Errorf("local-mode limit = %q, want web", m3.Limit)
	}
	if m3.Inventory != nil {
		t.Errorf("local-mode must NOT ship inventory, got %+v", m3.Inventory)
	}
}

// insertJobDef inserts a minimal jobs cache row so ResolveCommand + job knobs
// resolve.
func insertJobDef(t *testing.T, svc *Service, name, runType, command string, timeoutSeconds int) {
	t.Helper()
	_, err := svc.db.Exec(`
		INSERT INTO jobs(name, run_type, command, timeout_seconds, synced_at)
		VALUES (?, ?, ?, ?, ?)`,
		name, runType, command, timeoutSeconds, now())
	if err != nil {
		t.Fatalf("insertJobDef: %v", err)
	}
}

// seedScopeHost wires a scope → host → ssh_hosts record so ResolveTargets
// returns a real target (with an auth-key-env-var NAME, never key bytes).
func seedScopeHost(t *testing.T, svc *Service, scope, host, authKeyEnvVar string) {
	t.Helper()
	ts := now()
	scopeID := db.NewID()
	if _, err := svc.db.Exec(`
		INSERT INTO scopes(id, name, source, created_at) VALUES (?, ?, 'amadeus', ?)`,
		scopeID, scope, ts); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO scope_hosts(scope_id, host) VALUES (?, ?)`, scopeID, host); err != nil {
		t.Fatalf("seed scope_host: %v", err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO ssh_hosts(id, hostname, port, username, auth_key_env_var, created_at)
		VALUES (?, ?, 2222, 'deploy', ?, ?)`,
		db.NewID(), host, authKeyEnvVar, ts); err != nil {
		t.Fatalf("seed ssh_host: %v", err)
	}
}

// authSvc builds an auth.Service sharing the runner Service's DB so the real
// RequireRunner middleware (token→runner binding, R1.4) can be exercised.
func authSvc(t *testing.T, svc *Service) *auth.Service {
	t.Helper()
	cfg := &config.Config{SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}
	return auth.NewService(context.Background(), cfg, svc.db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestManifestOwnership is the R1.4 regression: a runner that does NOT own a run
// is rejected (404) by both /manifest and /log; the owning runner succeeds.
func TestManifestOwnership(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	ownerID, ownerTok := "runner-owner", "crn_run_owner"
	otherID, otherTok := "runner-other", "crn_run_other"
	insertRunner(t, svc, ownerID, "owner", "online", []string{"bash"})
	insertRunner(t, svc, otherID, "other", "online", []string{"bash"})
	bindRunnerToken(t, svc, ownerTok, ownerID)
	bindRunnerToken(t, svc, otherTok, otherID)

	traceID := db.NewTraceID()
	insertJobDef(t, svc, "owned-job", "bash", "echo hi", 0)
	insertClaimedRun(t, svc, traceID, "owned-job", "bash", "", ownerID, "")

	manifestH := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	logH := as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog))

	call := func(h http.Handler, method, suffix, token, body string) int {
		req := httptest.NewRequest(method, "/api/v1/runs/"+traceID+suffix, strings.NewReader(body))
		req.SetPathValue("traceId", traceID)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Non-owning runner → 404 on both.
	if got := call(manifestH, http.MethodGet, "/manifest", otherTok, ""); got != http.StatusNotFound {
		t.Errorf("non-owner manifest: got %d, want 404", got)
	}
	if got := call(logH, http.MethodPost, "/log", otherTok, "data\n"); got != http.StatusNotFound {
		t.Errorf("non-owner log: got %d, want 404", got)
	}

	// Owning runner → 200 manifest.
	if got := call(manifestH, http.MethodGet, "/manifest", ownerTok, ""); got != http.StatusOK {
		t.Errorf("owner manifest: got %d, want 200", got)
	}
	// Owning runner → 204 log (envelope closes the run).
	envelope := `{"exitCode":0,"durationMs":1,"endedAt":"2026-06-09T12:00:00Z"}` + "\n"
	if got := call(logH, http.MethodPost, "/log", ownerTok, envelope); got != http.StatusNoContent {
		t.Errorf("owner log: got %d, want 204", got)
	}
}

// TestManifestAmadeusMode verifies the amadeus inventory mode: targets are
// fully resolved and carry the auth-key env-var NAME only (D1 — never key bytes).
func TestManifestAmadeusMode(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-am", "crn_run_am"
	insertRunner(t, svc, runnerID, "am", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	seedScopeHost(t, svc, "prod", "web01", "PROD_DEPLOY_KEY")
	insertJobDef(t, svc, "deploy", "bash", "echo deploy", 120)

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "deploy", "bash", "prod", runnerID, `{"STAGE":"prod"}`)

	m := getManifest(t, svc, as, traceID, tok)

	if m.InventoryMode != "amadeus" {
		t.Errorf("inventoryMode = %q, want amadeus", m.InventoryMode)
	}
	if m.Scope != "prod" || m.RunType != "bash" || m.JobName != "deploy" {
		t.Errorf("manifest header mismatch: %+v", m)
	}
	if m.TimeoutSeconds != 120 {
		t.Errorf("job knobs not surfaced: timeout=%d, want 120", m.TimeoutSeconds)
	}
	if m.Env["STAGE"] != "prod" {
		t.Errorf("env snapshot not surfaced: %+v", m.Env)
	}
	if len(m.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d: %+v", len(m.Targets), m.Targets)
	}
	tg := m.Targets[0]
	if tg.Name != "web01" || tg.Port != 2222 || tg.User != "deploy" {
		t.Errorf("target not resolved: %+v", tg)
	}
	// D1: reference only — the env-var NAME, never key bytes.
	if tg.AuthKeyEnvVar != "PROD_DEPLOY_KEY" {
		t.Errorf("authKeyEnvVar = %q, want the env-var NAME PROD_DEPLOY_KEY", tg.AuthKeyEnvVar)
	}
	// RX.9: SSH run-types execute remotely with the manifest env alone; no
	// passthrough allowlist is computed (nil ⇒ agent keeps legacy behavior).
	if m.EnvPassthrough != nil {
		t.Errorf("bash run should carry no envPassthrough, got %v", m.EnvPassthrough)
	}
}

// TestManifestEnvPassthrough (RX.9, Phase 1): a local-toolchain (ansible) run's
// manifest carries the env-passthrough NAME allowlist — the sorted union of the
// job's env_passthrough list and the inventory's {{ lookup('env', NAME) }}
// references — and it is PRESENT (non-null) even when empty, because presence
// is what flips the agent to the scoped child env.
func TestManifestEnvPassthrough(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-envp", "crn_run_envp"
	insertRunner(t, svc, runnerID, "envp", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	raw := "[web]\nweb1 ansible_ssh_pass=\"{{ lookup('env','WEB_PASS') }}\"\n" +
		"web2 token=\"{{ lookup('ansible.builtin.env', 'API_TOKEN') }}\"\n"
	seedAnsibleScope(t, svc, "prod", raw)
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n  tasks: []\n", 0)
	if _, err := svc.db.Exec(
		`UPDATE jobs SET env_passthrough='["VCENTER_TOKEN","WEB_PASS"]' WHERE name='patch'`); err != nil {
		t.Fatalf("set env_passthrough: %v", err)
	}

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "patch", "ansible", "prod", runnerID, "")

	m := getManifest(t, svc, as, traceID, tok)
	want := []string{"API_TOKEN", "VCENTER_TOKEN", "WEB_PASS"} // deduped union, sorted
	if len(m.EnvPassthrough) != len(want) {
		t.Fatalf("envPassthrough = %v, want %v", m.EnvPassthrough, want)
	}
	for i := range want {
		if m.EnvPassthrough[i] != want[i] {
			t.Errorf("envPassthrough[%d] = %q, want %q", i, m.EnvPassthrough[i], want[i])
		}
	}
}

// TestManifestEnvPassthroughEmptyButPresent: an ansible run with no lookups and
// no job list still ships an EMPTY (not absent) allowlist — scoping must engage.
func TestManifestEnvPassthroughEmptyButPresent(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-envp0", "crn_run_envp0"
	insertRunner(t, svc, runnerID, "envp0", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	seedAnsibleScope(t, svc, "prod", "[web]\nweb1 ansible_host=10.0.0.1\n")
	insertJobDef(t, svc, "patch0", "ansible", "- hosts: web\n  tasks: []\n", 0)

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "patch0", "ansible", "prod", runnerID, "")

	// Decode the raw JSON to distinguish [] (present) from null/absent.
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: got %d; body: %s", rec.Code, rec.Body.String())
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &rawMap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(rawMap["envPassthrough"]) != "[]" {
		t.Errorf("envPassthrough = %s, want [] (present-but-empty)", rawMap["envPassthrough"])
	}
}

// TestManifestLocalMode verifies that a runner registered inventory=local gets
// the scope name but NO resolved targets (the agent resolves locally, D8/T-b).
func TestManifestLocalMode(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-local", "crn_run_local"
	insertRunner(t, svc, runnerID, "local", "online", []string{"bash"})
	// Flip this runner to local inventory.
	if _, err := svc.db.Exec(`UPDATE runners SET inventory='local' WHERE id=?`, runnerID); err != nil {
		t.Fatalf("set local inventory: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	// Even if scope hosts exist, local mode must NOT resolve them into targets.
	seedScopeHost(t, svc, "isolated", "secret-host", "ISO_KEY")
	insertJobDef(t, svc, "iso-job", "bash", "echo iso", 0)

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "iso-job", "bash", "isolated", runnerID, "")

	m := getManifest(t, svc, as, traceID, tok)
	if m.InventoryMode != "local" {
		t.Errorf("inventoryMode = %q, want local", m.InventoryMode)
	}
	if m.Scope != "isolated" {
		t.Errorf("scope = %q, want isolated", m.Scope)
	}
	if len(m.Targets) != 0 {
		t.Errorf("local mode must ship NO targets, got %d: %+v", len(m.Targets), m.Targets)
	}
}

// TestManifestConflictStates verifies non-running / non-runner runs are 409.
func TestManifestConflictStates(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-x", "crn_run_x"
	insertRunner(t, svc, runnerID, "x", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	insertJobDef(t, svc, "j", "bash", "echo j", 0)

	// A queued (not running) runner run.
	traceID := db.NewTraceID()
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, runner_id, executor, triggered_by, trigger_kind, created_at)
		VALUES (?, 'j', 'bash', 'queued', ?, 'runner', 'test', 'manual', ?)`,
		traceID, runnerID, ts); err != nil {
		t.Fatalf("insert queued run: %v", err)
	}

	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("queued run manifest: got %d, want 409", rec.Code)
	}
}

// seedAnsibleScope inserts a git-source scope carrying a raw inventory, as sync
// would after secret-rejection (M1).
func seedAnsibleScope(t *testing.T, svc *Service, scope, raw string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO scopes(id, name, source, created_at, raw_inventory, inventory_format)
		VALUES (?, ?, 'git', ?, ?, 'ini')`,
		db.NewID(), scope, now(), raw); err != nil {
		t.Fatalf("seed ansible scope: %v", err)
	}
}

// TestManifestAnsibleInventory: an amadeus-mode ansible run on a v2 agent ships
// the scope's byte-exact inventory for `-i` (M1 / §7.3).
func TestManifestAnsibleInventory(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-ans", "crn_run_ans"
	insertRunner(t, svc, runnerID, "ans", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	raw := "[web]\nweb1 ansible_host=10.0.0.1\n"
	seedAnsibleScope(t, svc, "prod", raw)
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n  tasks: []\n", 0)

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "patch", "ansible", "prod", runnerID, "")

	m := getManifest(t, svc, as, traceID, tok)
	if m.Inventory == nil {
		t.Fatalf("expected inventory shipped for amadeus ansible run, got nil")
	}
	if m.Inventory.Raw != raw {
		t.Errorf("inventory raw = %q, want byte-exact %q", m.Inventory.Raw, raw)
	}
	if m.Inventory.Format != "ini" {
		t.Errorf("inventory format = %q, want ini", m.Inventory.Format)
	}
}

// TestManifestLocalModeAnsibleNoInventory: local-mode no-leak (D2) — even with a
// scope carrying raw inventory, a local-mode runner is NEVER shipped it.
func TestManifestLocalModeAnsibleNoInventory(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-liso", "crn_run_liso"
	insertRunner(t, svc, runnerID, "liso", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET inventory='local', protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set local: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	seedAnsibleScope(t, svc, "isolated", "[secret]\nsecret-host\n")
	insertJobDef(t, svc, "iso-ansible", "ansible", "- hosts: secret\n", 0)

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "iso-ansible", "ansible", "isolated", runnerID, "")

	m := getManifest(t, svc, as, traceID, tok)
	if m.InventoryMode != "local" {
		t.Errorf("inventoryMode = %q, want local", m.InventoryMode)
	}
	if m.Inventory != nil {
		t.Errorf("local mode must NOT ship inventory (no-leak), got raw=%q", m.Inventory.Raw)
	}
}

// TestManifestAnsibleNoInventoryRejected: an amadeus ansible run against a scope
// with NO managed inventory hard-fails (409) rather than run unscoped (§7.3/§15).
func TestManifestAnsibleNoInventoryRejected(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-noi", "crn_run_noi"
	insertRunner(t, svc, runnerID, "noi", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	// A scope with hosts but NO raw inventory (an amadeus host-list scope, or one
	// not yet re-synced). seedScopeHost inserts a scope without raw_inventory.
	seedScopeHost(t, svc, "prod", "web01", "PROD_KEY")
	insertJobDef(t, svc, "patch", "ansible", "- hosts: web\n", 0)

	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "patch", "ansible", "prod", runnerID, "")

	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("ansible run with no managed inventory: got %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no managed inventory") {
		t.Errorf("expected a 'no managed inventory' message, got %s", rec.Body.String())
	}
}

// TestManifestTerraform covers the execspec.ResolveCommand change that newly lets
// local-toolchain run-types resolve a manifest (previously errored → 500). A
// terraform run ships its body with no inventory.
func TestManifestTerraform(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-tf", "crn_run_tf"
	insertRunner(t, svc, runnerID, "tf", "online", []string{"terraform"})
	bindRunnerToken(t, svc, tok, runnerID)

	insertJobDef(t, svc, "infra", "terraform", "plan -out=tfplan", 0)
	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "infra", "terraform", "", runnerID, "")

	m := getManifest(t, svc, as, traceID, tok)
	if m.RunType != "terraform" {
		t.Errorf("runType = %q, want terraform", m.RunType)
	}
	if m.Body != "plan -out=tfplan" {
		t.Errorf("body = %q, want the tf args", m.Body)
	}
	if m.Inventory != nil {
		t.Errorf("terraform must not carry inventory, got %+v", m.Inventory)
	}
}

// getManifest fetches and decodes a manifest through the real RequireRunner
// chain, asserting a 200.
func getManifest(t *testing.T, svc *Service, as *auth.Service, traceID, token string) runnerproto.ManifestResponse {
	t.Helper()
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: got %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var m runnerproto.ManifestResponse
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

// seedCheckoutRun inserts a project job + a claimed checkout run (pinned SHA +
// entry snapshotted onto the run, as enqueue would). The job has no scope, so
// the manifest skips the ansible-inventory attach and reaches the checkout path.
func seedCheckoutRun(t *testing.T, svc *Service, runnerID, sha, entry string) string {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO jobs(name, run_type, command, script_path, project_root, synced_at)
		VALUES ('proj', 'ansible', ?, ?, 'scripts/proj', ?)`,
		"- hosts: all\n", entry, now()); err != nil {
		t.Fatalf("insert project job: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, runner_id, executor,
		                 checkout_sha, checkout_entry, triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, 'proj', 'ansible', 'running', ?, 'runner', ?, ?, 'test', 'manual', ?, ?)`,
		traceID, runnerID, sha, entry, now(), now()); err != nil {
		t.Fatalf("insert checkout run: %v", err)
	}
	return traceID
}

// TestManifestCheckout (Phase 2 / §7): a project run on a v3 agent ships a
// ManifestCheckout carrying the resolved repo URL + pinned SHA + entry.
func TestManifestCheckout(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.GitLabBaseURL = "https://gitlab.example/infra/job-defs.git"
	as := authSvc(t, svc)

	runnerID, tok := "runner-co", "crn_run_co"
	insertRunner(t, svc, runnerID, "co", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	sha := "0123456789abcdef0123456789abcdef01234567"
	traceID := seedCheckoutRun(t, svc, runnerID, sha, "scripts/proj/site.yml")

	m := getManifest(t, svc, as, traceID, tok)
	if m.Checkout == nil {
		t.Fatalf("expected a checkout spec, got nil")
	}
	if m.Checkout.Repo != "https://gitlab.example/infra/job-defs.git" {
		t.Errorf("checkout repo = %q, want the resolved repo URL", m.Checkout.Repo)
	}
	if m.Checkout.SHA != sha {
		t.Errorf("checkout sha = %q, want the pinned %q", m.Checkout.SHA, sha)
	}
	if m.Checkout.Entry != "scripts/proj/site.yml" {
		t.Errorf("checkout entry = %q, want the pinned entry", m.Checkout.Entry)
	}
	// Body stays populated for display parity even though the agent ignores it.
	if m.Body == "" {
		t.Errorf("body should stay populated in checkout mode (display parity)")
	}
}

// TestManifestCheckoutVaultAndReqPath (Phase 3): a checkout run whose job is a
// project with requires:[vault] ships UsesVault + a ReqPath candidate derived
// from project_root.
func TestManifestCheckoutVaultAndReqPath(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.GitLabBaseURL = "https://gitlab.example/infra/job-defs.git"
	as := authSvc(t, svc)

	runnerID, tok := "runner-cv", "crn_run_cv"
	insertRunner(t, svc, runnerID, "cv", "online", []string{"ansible"})
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, runnerID); err != nil {
		t.Fatalf("set protocol_version: %v", err)
	}
	bindRunnerToken(t, svc, tok, runnerID)

	sha := "0123456789abcdef0123456789abcdef01234567"
	if _, err := svc.db.Exec(`
		INSERT INTO jobs(name, run_type, command, script_path, project_root, requires_json, synced_at)
		VALUES ('proj', 'ansible', ?, 'scripts/proj/site.yml', 'scripts/proj', '["vault"]', ?)`,
		"- hosts: all\n", now()); err != nil {
		t.Fatalf("insert project job: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, runner_id, executor,
		                 checkout_sha, checkout_entry, requires_json, triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, 'proj', 'ansible', 'running', ?, 'runner', ?, 'scripts/proj/site.yml', '["vault"]', 'test', 'manual', ?, ?)`,
		traceID, runnerID, sha, now(), now()); err != nil {
		t.Fatalf("insert checkout run: %v", err)
	}

	m := getManifest(t, svc, as, traceID, tok)
	if m.Checkout == nil {
		t.Fatalf("expected a checkout spec")
	}
	if !m.Checkout.UsesVault {
		t.Errorf("requires:[vault] should set Checkout.UsesVault")
	}
	if m.Checkout.ReqPath != "scripts/proj/requirements.yml" {
		t.Errorf("ReqPath = %q, want the project_root candidate scripts/proj/requirements.yml", m.Checkout.ReqPath)
	}
}
