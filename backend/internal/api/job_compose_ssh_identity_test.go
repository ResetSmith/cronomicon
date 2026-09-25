package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

// CA-9 (the ssh-user plan Phase B) — the Composer's declarative
// "connect as" identity: persisted + echoed for prefill, validated hard at
// authoring time (unknown label, run-type gate), and cleared by the JC1
// full-replace resend.
func TestJobComposeSSHIdentity(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('site-play','ansible','site.yml','runner','sha256:bbb','scripts/site-play.yaml','t')`)
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('infra-tf','terraform','apply','runner','sha256:ccc','scripts/infra-tf.yaml','t')`)
	seed(`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version,
	                                   created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('c1','prod-key','stored', x'00', x'00', x'00', 1, 't','t','t','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) (*http.Response, func()) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp, func() { resp.Body.Close() }
	}

	// Create with an identity: persisted + echoed in the create response.
	resp, done := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "id-compose", "scriptRef": "backup-db", "sshUser": "deploy", "sshCredential": "prod-key",
		"scope": "",
	})
	var created struct {
		ID            int64   `json:"id"`
		SSHUser       *string `json:"sshUser"`
		SSHCredential *string `json:"sshCredential"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	done()
	if created.SSHUser == nil || *created.SSHUser != "deploy" || created.SSHCredential == nil || *created.SSHCredential != "prod-key" {
		t.Errorf("create echo = (%v,%v), want (deploy,prod-key)", created.SSHUser, created.SSHCredential)
	}
	var jobUser, jobCred sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM jobs WHERE source='cronomicon' AND name='id-compose'`).Scan(&jobUser, &jobCred); err != nil {
		t.Fatalf("fetch job: %v", err)
	}
	if jobUser.String != "deploy" || jobCred.String != "prod-key" {
		t.Errorf("persisted identity = (%q,%q), want (deploy,prod-key)", jobUser.String, jobCred.String)
	}

	// Unknown label — hard 422 at authoring time (unlike the advisory Git sync).
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "id-bad", "scriptRef": "backup-db", "sshCredential": "no-such-key",
		"scope": "",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown label = %d, want 422", resp.StatusCode)
	}
	done()

	// Bad username charset — hard 422.
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "id-bad2", "scriptRef": "backup-db", "sshUser": "bad user;rm",
		"scope": "",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad username = %d, want 422", resp.StatusCode)
	}
	done()

	// RP-6 — an ansible-script job MAY carry an identity now: the override is
	// delivered as connection extra-vars, which beat the scope inventory.
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "id-ans", "scriptRef": "site-play", "sshUser": "deploy",
		"scope": "",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("ansible job with identity = %d, want 201", resp.StatusCode)
	}
	done()
	var ansUser sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user FROM jobs WHERE source='cronomicon' AND name='id-ans'`).Scan(&ansUser); err != nil {
		t.Fatalf("fetch ansible job: %v", err)
	}
	if ansUser.String != "deploy" {
		t.Errorf("persisted ansible identity = %q, want deploy", ansUser.String)
	}

	// RP-Q2 — terraform keeps the 422: its providers authenticate, so the field
	// could only no-op while the UI showed it as applied.
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "id-tf", "scriptRef": "infra-tf", "sshUser": "deploy",
		"scope": "",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("terraform job with identity = %d, want 422", resp.StatusCode)
	}
	done()

	// JC1 full-replace PUT with empty fields CLEARS the identity.
	resp, done = do(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(created.ID), map[string]any{
		"name": "id-compose", "scriptRef": "backup-db",
		"scope": "",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing PUT = %d, want 200", resp.StatusCode)
	}
	done()
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM jobs WHERE source='cronomicon' AND name='id-compose'`).Scan(&jobUser, &jobCred); err != nil {
		t.Fatalf("fetch job after clear: %v", err)
	}
	if jobUser.Valid || jobCred.Valid {
		t.Errorf("identity after clearing PUT = (%v,%v), want NULLs", jobUser, jobCred)
	}
}

// ── RP-14 · env_passthrough composer authoring ──────────────────────────────
//
// Git YAML has carried spec.env_passthrough since protocol v3, but the composer
// could not author it — so an in-app-composed ansible job could never resolve a
// runner-local variable into its child process.
func TestJobComposeEnvPassthrough(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('site-play','ansible','site.yml','runner','sha256:bbb','scripts/site-play.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) (*http.Response, func()) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp, func() { resp.Body.Close() }
	}

	// Local-toolchain job: accepted and persisted in the same serialization the
	// git sync path writes.
	resp, done := do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "ep-ans", "scriptRef": "site-play",
		"scope":          "",
		"envPassthrough": []string{"SITE_LICENCE_KEY", " VAULT_ADDR "},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ansible job with envPassthrough = %d, want 201", resp.StatusCode)
	}
	done()
	var stored string
	if err := pool.QueryRowContext(ctx,
		`SELECT env_passthrough FROM jobs WHERE source='cronomicon' AND name='ep-ans'`).Scan(&stored); err != nil {
		t.Fatalf("fetch job: %v", err)
	}
	if stored != `["SITE_LICENCE_KEY","VAULT_ADDR"]` {
		t.Errorf("persisted env_passthrough = %s, want the trimmed 2-name list", stored)
	}

	// ssh-family: 422. A bash run's remote env is built entirely from its
	// manifest, so accepting the field would be a silent no-op.
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "ep-bash", "scriptRef": "backup-db", "scope": "", "envPassthrough": []string{"FOO"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bash job with envPassthrough = %d, want 422", resp.StatusCode)
	}
	done()

	// CRONOMICON_* is reserved: those are references Cronomicon injects, not variables
	// read from the runner's environment.
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "ep-res", "scriptRef": "site-play", "scope": "", "envPassthrough": []string{"CRONOMICON_SECRET_X"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("reserved envPassthrough name = %d, want 422", resp.StatusCode)
	}
	done()

	// Not a valid env-var name at all.
	resp, done = do(http.MethodPost, ts.URL+"/api/v1/jobs", map[string]any{
		"name": "ep-bad", "scriptRef": "site-play", "scope": "", "envPassthrough": []string{"9nope"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("invalid envPassthrough name = %d, want 422", resp.StatusCode)
	}
	done()
}
