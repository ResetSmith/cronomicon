package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestRunPathEnvOverride verifies F1 (architecture-update.md §4): a manual run with
// an `env` override lands the override both in runs.env_json (the snapshot both
// executors inject) and in runs.override_json (the F3 audit envelope). Dev-login is
// an unrestricted admin, so the F-P2 scope guard does not interfere.
func TestRunPathEnvOverride(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
		 VALUES('envjob','git','bash','echo hi','Prod','sha256:e','jobs/envjob.yaml','t')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name='envjob'`).Scan(&rowid); err != nil {
		t.Fatalf("rowid: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	reqBody, _ := json.Marshal(map[string]any{"env": map[string]string{"FOO": "barbarbar"}})
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/v1/jobs/%d/run", ts.URL, rowid), bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST run = %d, want 202", resp.StatusCode)
	}

	var envJSON, overrideJSON string
	if err := pool.QueryRowContext(ctx,
		`SELECT COALESCE(env_json,''), COALESCE(override_json,'') FROM runs WHERE job_name='envjob'`,
	).Scan(&envJSON, &overrideJSON); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if envJSON != `{"FOO":"barbarbar"}` {
		t.Errorf("env_json = %q, want the override injected", envJSON)
	}
	if !strings.Contains(overrideJSON, `"env"`) || !strings.Contains(overrideJSON, `"FOO":"barbarbar"`) {
		t.Errorf("override_json = %q, want it to record the env override", overrideJSON)
	}
}

// TestRunPathHostSubset verifies F2 (architecture-update.md §5): a manual run with a
// targetHosts subset is rejected 422 when a host is not a member of the bound scope,
// accepted 202 for an in-scope subset, and the chosen subset is recorded in
// runs.override_json.hosts.
func TestRunPathHostSubset(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	scopeID := db.NewID()
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES (?, 'Fleet', 'amadeus', 't')`, scopeID)
	for _, h := range []string{"node1", "node2"} {
		exec(`INSERT INTO scope_hosts(scope_id, host) VALUES (?, ?)`, scopeID, h)
		exec(`INSERT INTO ssh_hosts(id, hostname, port, username, auth_key_env_var, created_at)
		      VALUES (?, ?, 22, 'deploy', '', 't')`, db.NewID(), h)
	}
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('fleetjob','git','bash','echo hi','Fleet','sha256:f','jobs/fleetjob.yaml','t')`)
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name='fleetjob'`).Scan(&rowid); err != nil {
		t.Fatalf("rowid: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	run := func(hosts []string) (int, string) {
		t.Helper()
		reqBody, _ := json.Marshal(map[string]any{"targetHosts": hosts})
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/jobs/%d/run", ts.URL, rowid), bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// A non-member host → 422 (the run is never enqueued).
	if code, body := run([]string{"node1", "rogue"}); code != http.StatusUnprocessableEntity {
		t.Errorf("non-member subset = %d (%s), want 422", code, body)
	}
	// An in-scope subset → 202, recorded in override_json.hosts.
	if code, body := run([]string{"node1"}); code != http.StatusAccepted {
		t.Fatalf("member subset = %d (%s), want 202", code, body)
	}
	var overrideJSON string
	if err := pool.QueryRowContext(ctx,
		`SELECT COALESCE(override_json,'') FROM runs WHERE job_name='fleetjob' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&overrideJSON); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if !strings.Contains(overrideJSON, `"hosts"`) || !strings.Contains(overrideJSON, "node1") {
		t.Errorf("override_json = %q, want it to record the host subset", overrideJSON)
	}
}

// TestRunPathGroupSubset (M3): group targeting is validated at the trigger
// boundary — unknown group → 422, degraded projection → 422, a valid group → 202
// recorded in override_json.groups; and a raw ansibleLimit is 422'd on an SSH job.
func TestRunPathGroupSubset(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Scope with a parsed (ok) projection + group "web".
	scopeID := db.NewID()
	exec(`INSERT INTO scopes(id,name,source,created_at,projection_status) VALUES (?,'Fleet','amadeus','t','ok')`, scopeID)
	for _, h := range []string{"node1", "node2"} {
		exec(`INSERT INTO scope_hosts(scope_id,host) VALUES (?,?)`, scopeID, h)
		exec(`INSERT INTO ssh_hosts(id,hostname,port,username,auth_key_env_var,created_at) VALUES (?,?,22,'deploy','','t')`, db.NewID(), h)
	}
	exec(`INSERT INTO scope_groups(scope_id,name) VALUES (?,'web')`, scopeID)
	exec(`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES (?,'web','node1')`, scopeID)
	// Scope with a degraded projection.
	brokenID := db.NewID()
	exec(`INSERT INTO scopes(id,name,source,created_at,projection_status) VALUES (?,'Broken','amadeus','t','unavailable')`, brokenID)
	exec(`INSERT INTO scope_hosts(scope_id,host) VALUES (?,?)`, brokenID, "bnode")
	exec(`INSERT INTO ssh_hosts(id,hostname,port,username,auth_key_env_var,created_at) VALUES (?,?,22,'deploy','','t')`, db.NewID(), "bnode")

	exec(`INSERT INTO jobs(name,source,run_type,command,scope,content_hash,source_path,synced_at)
	      VALUES('grpjob','git','bash','echo hi','Fleet','sha256:f','jobs/grpjob.yaml','t')`)
	exec(`INSERT INTO jobs(name,source,run_type,command,scope,content_hash,source_path,synced_at)
	      VALUES('brokenjob','git','bash','echo hi','Broken','sha256:b','jobs/brokenjob.yaml','t')`)
	exec(`INSERT INTO jobs(name,source,run_type,command,content_hash,source_path,synced_at)
	      VALUES('noscopejob','git','bash','echo hi','sha256:n','jobs/noscopejob.yaml','t')`) // no bound scope
	rowid := func(name string) int64 {
		var id int64
		if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name=?`, name).Scan(&id); err != nil {
			t.Fatalf("rowid %s: %v", name, err)
		}
		return id
	}
	grp, broken, noscope := rowid("grpjob"), rowid("brokenjob"), rowid("noscopejob")

	client, csrf := devLoginWithCSRF(t, ts)
	run := func(rid int64, bodyMap map[string]any) (int, string) {
		t.Helper()
		reqBody, _ := json.Marshal(bodyMap)
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/jobs/%d/run", ts.URL, rid), bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Unknown group → 422 group_membership.
	if code, body := run(grp, map[string]any{"targetGroups": []string{"nope"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "group_membership") {
		t.Errorf("unknown group = %d (%s), want 422 group_membership", code, body)
	}
	// targetGroups with no bound scope → 422 group_membership (requires a scope).
	if code, body := run(noscope, map[string]any{"targetGroups": []string{"web"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "group_membership") {
		t.Errorf("group with no scope = %d (%s), want 422 group_membership", code, body)
	}
	// Degraded projection → 422 (reject all group targeting).
	if code, body := run(broken, map[string]any{"targetGroups": []string{"web"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("degraded-projection group = %d (%s), want 422", code, body)
	}
	// Raw ansibleLimit on an SSH (bash) job → 422 invalid_executor.
	if code, body := run(grp, map[string]any{"ansibleLimit": "web:&staged"}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "invalid_executor") {
		t.Errorf("ansibleLimit on ssh = %d (%s), want 422 invalid_executor", code, body)
	}
	// Valid group → 202, recorded in override_json.groups.
	if code, body := run(grp, map[string]any{"targetGroups": []string{"web"}}); code != http.StatusAccepted {
		t.Fatalf("valid group = %d (%s), want 202", code, body)
	}
	var overrideJSON string
	if err := pool.QueryRowContext(ctx,
		`SELECT COALESCE(override_json,'') FROM runs WHERE job_name='grpjob' ORDER BY created_at DESC LIMIT 1`).
		Scan(&overrideJSON); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if !strings.Contains(overrideJSON, `"groups"`) || !strings.Contains(overrideJSON, "web") {
		t.Errorf("override_json = %q, want it to record the group subset", overrideJSON)
	}
}

// TestRunPathAnsibleTargetHostMetachar is the TG-4 manual-trigger boundary: a
// targetHost carrying an ansible pattern metacharacter (e.g. "web[01:50]") is
// passed verbatim to `ansible --limit`, where the metachar is silently DROPPED —
// leaving no --limit at all and widening the run to the full inventory. runJob
// must refuse this at the boundary (422 validation_failed) and enqueue NO run,
// rather than let it dispatch and die on the manifest's 409 backstop.
//
// The check is scoped to where the pin is actually used: a raw ansibleLimit
// passthrough in the same request is the documented escape hatch (the operator
// takes manual control of --limit), so that branch must NOT 422. A plain host
// name must still succeed normally.
func TestRunPathAnsibleTargetHostMetachar(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('pinjob','git','ansible','- hosts: web\n','sha256:p','jobs/pinjob.yaml','t')`)
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name='pinjob'`).Scan(&rowid); err != nil {
		t.Fatalf("rowid: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	run := func(bodyMap map[string]any) (int, string) {
		t.Helper()
		reqBody, _ := json.Marshal(bodyMap)
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/jobs/%d/run", ts.URL, rowid), bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	runCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_name='pinjob'`).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}

	// A metachar targetHost → 422 validation_failed, and NO run is enqueued.
	if code, body := run(map[string]any{"targetHost": "web[01:50]"}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "validation_failed") {
		t.Errorf("metachar targetHost = %d (%s), want 422 validation_failed", code, body)
	}
	if n := runCount(); n != 0 {
		t.Errorf("runs for pinjob = %d after rejected trigger, want 0", n)
	}

	// The SAME metachar targetHost, but with a raw ansibleLimit passthrough → the
	// guard is deliberately skipped (the operator has taken manual control of
	// --limit), so this must NOT 422.
	if code, body := run(map[string]any{"targetHost": "web[01:50]", "ansibleLimit": "web:&staged"}); code != http.StatusAccepted {
		t.Errorf("metachar targetHost + ansibleLimit passthrough = %d (%s), want 202", code, body)
	}
	if n := runCount(); n != 1 {
		t.Errorf("runs for pinjob = %d after ansibleLimit-passthrough trigger, want 1", n)
	}

	// A plain host name still succeeds normally.
	if code, body := run(map[string]any{"targetHost": "web1"}); code != http.StatusAccepted {
		t.Errorf("plain targetHost = %d (%s), want 202", code, body)
	}
	if n := runCount(); n != 2 {
		t.Errorf("runs for pinjob = %d after plain-targetHost trigger, want 2", n)
	}
}
