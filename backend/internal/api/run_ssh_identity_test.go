package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// CA (the ssh-user plan) — the per-run "connect as" identity fields on
// POST /jobs/{id}/run: trigger-boundary validation, names-only persistence
// (runs columns + F3 envelope), and the ManageEnvVars gate on key selection.

// identityRun POSTs a run body through the shared triggerRun helper
// (integration_test.go) and returns the status code plus trace id.
func identityRun(t *testing.T, ts *httptest.Server, client *http.Client, csrf string, jobID int64, body map[string]any) (int, string) {
	t.Helper()
	code, resp := triggerRun(t, client, csrf, ts.URL, jobID, body)
	traceID, _ := resp["traceId"].(string)
	return code, traceID
}

func TestRunSSHIdentityPersisted(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	seedIdentityFixture(t, pool, "bash")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	code, traceID := identityRun(t, ts, client, csrf, jobID, map[string]any{
		"sshUser": "deploy", "sshCredential": "prod-key",
	})
	if code != http.StatusAccepted {
		t.Fatalf("run with identity = %d, want 202", code)
	}

	var sshUser, sshCred, override string
	if err := pool.QueryRowContext(ctx,
		`SELECT COALESCE(ssh_user,''), COALESCE(ssh_credential,''), COALESCE(override_json,'') FROM runs WHERE id=?`,
		traceID).Scan(&sshUser, &sshCred, &override); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser != "deploy" || sshCred != "prod-key" {
		t.Errorf("frozen identity = (%q,%q), want (deploy,prod-key)", sshUser, sshCred)
	}
	// JC12 — the envelope records names only.
	if !strings.Contains(override, `"sshUser":"deploy"`) || !strings.Contains(override, `"sshCredential":"prod-key"`) {
		t.Errorf("override_json = %q, want the identity recorded", override)
	}
	if strings.Contains(override, "PEM") || strings.Contains(override, "PRIVATE KEY") {
		t.Errorf("override_json must never carry key material: %q", override)
	}

	// Omitting both keeps the exact pre-CA shape: NULL columns, no envelope keys.
	code, traceID = identityRun(t, ts, client, csrf, jobID, map[string]any{})
	if code != http.StatusAccepted {
		t.Fatalf("plain run = %d, want 202", code)
	}
	var nUser, nCred any
	_ = pool.QueryRowContext(ctx, `SELECT ssh_user, ssh_credential FROM runs WHERE id=?`, traceID).Scan(&nUser, &nCred)
	if nUser != nil || nCred != nil {
		t.Errorf("plain run identity columns = (%v,%v), want NULLs", nUser, nCred)
	}
}

func TestRunSSHIdentityValidation(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "bash")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	// Username charset is the conservative POSIX-ish set (it becomes an SSH auth
	// string and rides audit surfaces verbatim).
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshUser": "bad user;rm"}); code != http.StatusUnprocessableEntity {
		t.Errorf("bad username = %d, want 422", code)
	}
	// The label must exist at trigger time — fail fast in the dialog.
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshCredential": "no-such-key"}); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown credential label = %d, want 422", code)
	}
}

// RP-6 — the run-type gate is IdentityCapableRunType, not "ssh-family": ansible
// accepts an identity override (delivered as connection extra-vars, which beat
// the inventory), terraform still refuses it because it authenticates through
// its providers and the fields could only no-op there.
func TestRunSSHIdentityRunTypeGate(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "ansible")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshUser": "deploy"}); code != http.StatusAccepted {
		t.Errorf("ansible + sshUser = %d, want 202", code)
	}
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshCredential": "prod-key"}); code != http.StatusAccepted {
		t.Errorf("ansible + sshCredential = %d, want 202", code)
	}
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{}); code != http.StatusAccepted {
		t.Errorf("ansible plain run = %d, want 202 (gate must not catch plain runs)", code)
	}

	// The ansible run must actually FREEZE what it accepted — accepting the field
	// and then dropping it would be the silent-wrong-identity failure the whole
	// override path exists to prevent.
	code, traceID := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshUser": "deploy", "sshCredential": "prod-key"})
	if code != http.StatusAccepted {
		t.Fatalf("ansible identity run = %d, want 202", code)
	}
	var user, cred string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT COALESCE(ssh_user,''), COALESCE(ssh_credential,'') FROM runs WHERE id=?`, traceID).Scan(&user, &cred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if user != "deploy" || cred != "prod-key" {
		t.Errorf("frozen ansible identity = (%q,%q), want (deploy,prod-key)", user, cred)
	}
}

// RP-Q2 — terraform is the one run type that keeps the 422: its providers do the
// authenticating, so an accepted sshUser could only be a no-op the UI would
// nonetheless display as applied.
func TestRunSSHIdentityTerraformStillRefused(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "terraform")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshUser": "deploy"}); code != http.StatusUnprocessableEntity {
		t.Errorf("terraform + sshUser = %d, want 422", code)
	}
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"sshCredential": "prod-key"}); code != http.StatusUnprocessableEntity {
		t.Errorf("terraform + sshCredential = %d, want 422", code)
	}
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{}); code != http.StatusAccepted {
		t.Errorf("terraform plain run = %d, want 202 (gate must not catch plain runs)", code)
	}
}

// TestRunSSHIdentityPermGate — CA-Q1: picking a stored key is a grant over stored
// key material, so it needs ManageEnvVars (the V2-11 rule). A username override
// alone is not material and stays open to any run-triggering role.
func TestRunSSHIdentityPermGate(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "run_id_rbac.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// RB-15: the grant is what authorizes. These cases are about the manageEnvVars
	// PERMISSION gate, not about scoping, so both groups get an unrestricted "*"
	// grant and the scope axis stays out of the way.
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('g0','id-operators','operator',NULL,1,'2026-01-01T00:00:00Z'),
		('g1','id-admins','admin',NULL,1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('id-job','git','bash','echo x','','sha256:a','jobs/id.yaml','t')`)
	exec(`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version,
	                                   created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('c1','prod-key','stored', x'00', x'00', x'00', 1, 't','t','t','t')`)
	var jobID int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name='id-job'`).Scan(&jobID); err != nil {
		t.Fatalf("rowid: %v", err)
	}

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()

	run := func(group string, body any) int {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/run", rdr)
		req.Header.Set("Remote-User", group+"@example.com")
		req.Header.Set("Remote-Groups", group)
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "tok"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := run("id-operators", map[string]any{"sshCredential": "prod-key"}); code != http.StatusForbidden {
		t.Errorf("operator run with sshCredential = %d, want 403 (needs ManageEnvVars)", code)
	}
	if code := run("id-operators", map[string]any{"sshUser": "deploy"}); code != http.StatusAccepted {
		t.Errorf("operator run with sshUser only = %d, want 202 (a username is not material)", code)
	}
	if code := run("id-admins", map[string]any{"sshCredential": "prod-key"}); code != http.StatusAccepted {
		t.Errorf("admin run with sshCredential = %d, want 202", code)
	}
}

// seedIdentityFixture inserts the job under test plus a stored credential whose
// label the tests select. The credential's sealed blobs are placeholders — the
// trigger boundary only checks label existence, never decrypts.
func seedIdentityFixture(t *testing.T, pool *sql.DB, runType string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
		 VALUES('idjob','git',?,'echo hi','','sha256:i','jobs/idjob.yaml','t')`, runType); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version,
		                              created_by, created_at, last_modified_by, last_modified_at)
		 VALUES ('cred1','prod-key','stored', x'00', x'00', x'00', 1, 't','t','t','t')`); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
}

// jobRowID returns the rowid the run endpoint addresses a job by.
func jobRowID(t *testing.T, pool *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name=?`, name).Scan(&id); err != nil {
		t.Fatalf("rowid of %q: %v", name, err)
	}
	return id
}

// TestRunJobSpecIdentityFold — CA-10 Phase B: the job-spec identity folds onto
// the run at the manual trigger (definition, not override: the F3 envelope stays
// operator-only), a per-run override wins per field, and an ansible job's stored
// identity is ignored rather than frozen.
func TestRunJobSpecIdentityFold(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version,
	                                   created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('c1','prod-key','stored', x'00', x'00', x'00', 1, 't','t','t','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, ssh_user, ssh_credential, synced_at)
	      VALUES('foldjob','git','bash','echo hi','','sha256:f','jobs/foldjob.yaml','svc','prod-key','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, ssh_user, ssh_credential, synced_at)
	      VALUES('foldans','git','ansible','play','','sha256:g','jobs/foldans.yaml','svc','prod-key','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, ssh_user, ssh_credential, synced_at)
	      VALUES('foldtf','git','terraform','apply','','sha256:h','jobs/foldtf.yaml','svc','prod-key','t')`)

	client, csrf := devLoginWithCSRF(t, ts)

	// Plain run: the job-spec identity lands on the frozen columns, and the
	// envelope records nothing (nothing was overridden).
	code, traceID := identityRun(t, ts, client, csrf, jobRowID(t, pool, "foldjob"), map[string]any{})
	if code != http.StatusAccepted {
		t.Fatalf("plain run = %d, want 202", code)
	}
	var sshUser, sshCred, override sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential, override_json FROM runs WHERE id=?`, traceID).Scan(&sshUser, &sshCred, &override); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.String != "svc" || sshCred.String != "prod-key" {
		t.Errorf("folded identity = (%q,%q), want (svc,prod-key)", sshUser.String, sshCred.String)
	}
	if strings.Contains(override.String, "sshUser") || strings.Contains(override.String, "sshCredential") {
		t.Errorf("override_json = %q, want no identity keys for a job-spec fold", override.String)
	}

	// Per-run override wins PER FIELD: the typed user replaces the job's, the
	// job's credential still folds beneath it.
	code, traceID = identityRun(t, ts, client, csrf, jobRowID(t, pool, "foldjob"), map[string]any{"sshUser": "override-user"})
	if code != http.StatusAccepted {
		t.Fatalf("override run = %d, want 202", code)
	}
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential, override_json FROM runs WHERE id=?`, traceID).Scan(&sshUser, &sshCred, &override); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.String != "override-user" || sshCred.String != "prod-key" {
		t.Errorf("merged identity = (%q,%q), want (override-user,prod-key)", sshUser.String, sshCred.String)
	}
	if !strings.Contains(override.String, `"sshUser":"override-user"`) || strings.Contains(override.String, "sshCredential") {
		t.Errorf("override_json = %q, want only the operator's sshUser recorded", override.String)
	}

	// RP-7 — an ansible job's stored identity now FOLDS like any ssh-family job:
	// the run carries it, and the dispatch path applies it as extra-vars.
	code, traceID = identityRun(t, ts, client, csrf, jobRowID(t, pool, "foldans"), map[string]any{})
	if code != http.StatusAccepted {
		t.Fatalf("ansible plain run = %d, want 202", code)
	}
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM runs WHERE id=?`, traceID).Scan(&sshUser, &sshCred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.String != "svc" || sshCred.String != "prod-key" {
		t.Errorf("ansible folded identity = (%q,%q), want (svc,prod-key)", sshUser.String, sshCred.String)
	}

	// RP-Q2 — terraform is still excluded, and (the point of testing the FOLD
	// rather than only the 422) a stored value reaching the row via Git sync must
	// be dropped here rather than frozen onto a run nothing would apply it to.
	code, traceID = identityRun(t, ts, client, csrf, jobRowID(t, pool, "foldtf"), map[string]any{})
	if code != http.StatusAccepted {
		t.Fatalf("terraform plain run = %d, want 202", code)
	}
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM runs WHERE id=?`, traceID).Scan(&sshUser, &sshCred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.Valid || sshCred.Valid {
		t.Errorf("terraform run identity = (%v,%v), want NULLs (not folded)", sshUser, sshCred)
	}
}

// ── Phase 3 · advanced ansible options (RP-15) ──────────────────────────────

// ansibleOptsRun triggers a run with an arbitrary body and returns the status +
// the resulting run's override envelope.
func ansibleOptsRun(t *testing.T, ts *httptest.Server, pool *sql.DB, client *http.Client, csrf string, jobID int64, body map[string]any) (int, string) {
	t.Helper()
	code, traceID := identityRun(t, ts, client, csrf, jobID, body)
	if code != http.StatusAccepted {
		return code, ""
	}
	var override sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT override_json FROM runs WHERE id=?`, traceID).Scan(&override); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	return code, override.String
}

func TestRunAnsibleOptionsRecorded(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "ansible")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	code, override := ansibleOptsRun(t, ts, pool, client, csrf, jobID, map[string]any{
		"ansibleCheck": true, "ansibleDiff": true,
		"ansibleTags": []string{"certs", "config"}, "ansibleSkipTags": []string{"reboot"},
		"ansibleVerbosity": 2, "ansibleBecome": true, "ansibleBecomeUser": "svc",
		"ansibleExtraVars": map[string]string{"env_name": "staging"},
	})
	if code != http.StatusAccepted {
		t.Fatalf("ansible options run = %d, want 202", code)
	}
	for _, want := range []string{
		`"ansibleCheck":true`, `"ansibleDiff":true`,
		`"ansibleTags":["certs","config"]`, `"ansibleSkipTags":["reboot"]`,
		`"ansibleVerbosity":2`, `"ansibleBecome":true`, `"ansibleBecomeUser":"svc"`,
		`"env_name":"staging"`,
	} {
		if !strings.Contains(override, want) {
			t.Errorf("override_json missing %s: %s", want, override)
		}
	}

	// A run that sets nothing must leave the envelope exactly as it was before
	// Phase 3 existed — no empty keys, no `false` noise.
	_, plain := ansibleOptsRun(t, ts, pool, client, csrf, jobID, map[string]any{})
	if strings.Contains(plain, "ansible") {
		t.Errorf("plain run envelope grew ansible keys: %s", plain)
	}
}

// The whole option set is ansible-only, for the ansibleLimit reason: any other
// toolchain would accept the field and silently drop it.
func TestRunAnsibleOptionsRunTypeGate(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "bash")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"ansibleCheck": true}); code != http.StatusUnprocessableEntity {
		t.Errorf("bash + ansibleCheck = %d, want 422", code)
	}
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"ansibleTags": []string{"x"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("bash + ansibleTags = %d, want 422", code)
	}
	// The gate keys on "did the operator set anything", so an explicit false must
	// NOT trip it — a client that always sends the field stays working.
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{"ansibleCheck": false}); code != http.StatusAccepted {
		t.Errorf("bash + ansibleCheck:false = %d, want 202 (unset must not trip the gate)", code)
	}
}

func TestRunAnsibleOptionsValidation(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "ansible")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	cases := []struct {
		name string
		body map[string]any
	}{
		// A comma inside a tag would silently re-split the selection once joined.
		{"comma in tag", map[string]any{"ansibleTags": []string{"a,b"}}},
		{"pattern char in skip tag", map[string]any{"ansibleSkipTags": []string{"web:!prod"}}},
		{"verbosity too high", map[string]any{"ansibleVerbosity": 9}},
		{"verbosity negative", map[string]any{"ansibleVerbosity": -1}},
		{"bad become user", map[string]any{"ansibleBecomeUser": "bad user;rm"}},
		{"bad extra-var name", map[string]any{"ansibleExtraVars": map[string]string{"9bad": "x"}}},
	}
	for _, tc := range cases {
		if code, _ := identityRun(t, ts, client, csrf, jobID, tc.body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", tc.name, code)
		}
	}
}

// RP-15 — `-e` is the tier the connect-as identity uses, and ansible takes the
// LAST occurrence of a repeated var. An operator extra-var naming one of the two
// identity vars is therefore refused outright: accepting it would let a run
// connect as someone other than what its own audit record claims.
func TestRunAnsibleExtraVarsCannotForgeIdentity(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "ansible")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	for _, name := range []string{"ansible_user", "ansible_ssh_private_key_file"} {
		if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{
			"ansibleExtraVars": map[string]string{name: "root"},
		}); code != http.StatusUnprocessableEntity {
			t.Errorf("extra-var %s = %d, want 422", name, code)
		}
	}
	// Every OTHER ansible_* name stays allowed — ansible_python_interpreter and
	// friends are legitimate and common.
	if code, _ := identityRun(t, ts, client, csrf, jobID, map[string]any{
		"ansibleExtraVars": map[string]string{"ansible_python_interpreter": "/usr/bin/python3"},
	}); code != http.StatusAccepted {
		t.Errorf("ansible_python_interpreter = %d, want 202 (only the identity vars are reserved)", code)
	}
}

// RV — the reviewed-sections audit record: known section names land in the
// envelope; caller-invented ones are silently dropped (the envelope is
// displayed verbatim, so it must not accumulate junk), and an empty result
// writes no key at all.
func TestRunReviewedSectionsRecorded(t *testing.T) {
	ts, pool := newTestServer(t)
	seedIdentityFixture(t, pool, "bash")
	client, csrf := devLoginWithCSRF(t, ts)
	jobID := jobRowID(t, pool, "idjob")

	code, override := ansibleOptsRun(t, ts, pool, client, csrf, jobID, map[string]any{
		"reviewedSections": []string{"variables", "where-it-runs", "when-to-run", "not-a-section"},
	})
	if code != http.StatusAccepted {
		t.Fatalf("run with reviewedSections = %d, want 202", code)
	}
	// RU-8 — "when-to-run" joined the allowlist; the filter preserves the
	// caller's order, so this also pins that the new name is not appended.
	if !strings.Contains(override, `"reviewedSections":["variables","where-it-runs","when-to-run"]`) {
		t.Errorf("override_json = %s, want the three known sections recorded", override)
	}
	if strings.Contains(override, "not-a-section") {
		t.Errorf("caller-invented section name leaked into the envelope: %s", override)
	}

	// Only unknown names ⇒ no key, keeping the envelope byte-identical to a
	// pre-RV run.
	_, override = ansibleOptsRun(t, ts, pool, client, csrf, jobID, map[string]any{
		"reviewedSections": []string{"junk"},
	})
	if strings.Contains(override, "reviewedSections") {
		t.Errorf("unknown-only reviewedSections grew an envelope key: %s", override)
	}
}
