package api_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// devLogin performs the dev-login bypass and returns a cookie-jar client plus
// the CSRF token, mirroring the pattern in TestSettingsEndpointsIntegration.
func devLogin(t *testing.T, tsURL string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout:       10 * time.Second,
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(tsURL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login: %v", err)
	}
	resp.Body.Close()
	u, _ := url.Parse(tsURL)
	var csrf string
	for _, c := range jar.Cookies(u) {
		if c.Name == "cronomicon_csrf" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF cookie after dev-login")
	}
	return client, csrf
}

// TestSshHostTestEndpoint exercises the "Test connection" handler contract
// (ssh-update.md TC.4): a probe that runs but fails is still HTTP 200 with the
// outcome as data, the status is persisted, CSRF is enforced, and an unknown id
// is 404. The host names an auth key that resolves to nothing, so the probe is
// cred_error without any dial — deterministic and offline. (A host with NO key
// at all would now take the TT reachability tier and dial.)
func TestSshHostTestEndpoint(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLogin(t, ts.URL)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, created_at)
		VALUES('h1','myhost','10.0.0.9',22,'tester','MISSING_KEY',?)`, now); err != nil {
		t.Fatal(err)
	}

	// Baseline: the freshly inserted row defaults to unverified.
	var status string
	_ = pool.QueryRow(`SELECT status FROM ssh_hosts WHERE id='h1'`).Scan(&status)
	if status != "unverified" {
		t.Fatalf("seeded status = %q, want unverified", status)
	}

	// Missing CSRF token ⇒ 403.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/hosts/h1/test", nil)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", r.StatusCode)
	}

	// With CSRF ⇒ 200, ran-and-failed (no key ⇒ cred_error).
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/hosts/h1/test", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /ssh/hosts/h1/test = %d, want 200", r.StatusCode)
	}

	// Status + last_checked_at persisted from the probe outcome.
	var lastChecked *string
	if err := pool.QueryRow(`SELECT status, last_checked_at FROM ssh_hosts WHERE id='h1'`).Scan(&status, &lastChecked); err != nil {
		t.Fatal(err)
	}
	if status != "cred_error" {
		t.Fatalf("persisted status = %q, want cred_error", status)
	}
	if lastChecked == nil || *lastChecked == "" {
		t.Error("last_checked_at not persisted after test")
	}

	// Unknown id ⇒ 404.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/hosts/ghost/test", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("POST unknown host = %d, want 404", r.StatusCode)
	}
}

// TestSshTestRecordsRun verifies V1.1-2: running a host "Test connection" also
// records it as a History/Executions run (kind='ssh-test') with the probe output
// stored as the run's Log Output. The seeded host names an auth key that
// resolves to nothing, so the probe is cred_error deterministically (offline) —
// a terminal failure run. (A truly keyless host would take the TT reachability
// tier and dial.)
func TestSshTestRecordsRun(t *testing.T) {
	ts, pool := newTestServerWithLogDir(t, t.TempDir())
	client, csrf := devLogin(t, ts.URL)

	now := time.Now().UTC().Format(time.RFC3339)
	// Audit columns are seeded so GetSshHost scans cleanly and resolves the display
	// name (recordhost) — exercising that the host's name, not its id, flows into the run.
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var,
		                      created_by, created_at, last_modified_by, last_modified_at)
		VALUES('hr1','recordhost','10.0.0.7',22,'tester','MISSING_KEY','tester',?,'tester',?)`, now, now); err != nil {
		t.Fatal(err)
	}

	// No ssh-test run exists before the test.
	var before int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind='ssh-test'`).Scan(&before)
	if before != 0 {
		t.Fatalf("pre-existing ssh-test runs = %d, want 0", before)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/hosts/hr1/test", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /ssh/hosts/hr1/test = %d, want 200", r.StatusCode)
	}

	// A terminal ssh-test run for this target now exists.
	var traceID, status, jobName, runType, target string
	err = pool.QueryRow(`
		SELECT id, status, job_name, run_type, target_host
		FROM runs WHERE kind='ssh-test' AND target_host='recordhost'`).
		Scan(&traceID, &status, &jobName, &runType, &target)
	if err != nil {
		t.Fatalf("expected an ssh-test run row: %v", err)
	}
	if status != "failure" {
		t.Errorf("ssh-test run status = %q, want failure (no auth key ⇒ cred_error)", status)
	}
	if runType != "bash" {
		t.Errorf("ssh-test run run_type = %q, want bash (CHECK floor)", runType)
	}
	if jobName != "SSH Test — recordhost" {
		t.Errorf("ssh-test run job_name = %q, want %q", jobName, "SSH Test — recordhost")
	}

	// An activity run-end row mirrors the run.
	var actCount int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind='run-end' AND trace_id=?`, traceID).Scan(&actCount)
	if actCount != 1 {
		t.Errorf("activity run-end rows for trace = %d, want 1", actCount)
	}

	// The probe output is served as the run's Log Output via GET /runs/{id}/log.
	logReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+traceID+"/log", nil)
	logResp, err := client.Do(logReq)
	if err != nil {
		t.Fatal(err)
	}
	defer logResp.Body.Close()
	if logResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /runs/%s/log = %d, want 200", traceID, logResp.StatusCode)
	}
	body, _ := io.ReadAll(logResp.Body)
	if len(strings.TrimSpace(string(body))) == 0 {
		t.Error("ssh-test run log is empty, want probe output")
	}
	if !strings.Contains(string(body), "recordhost") {
		t.Errorf("ssh-test run log missing target name; got:\n%s", body)
	}
}

// TestSshBastionTestEndpoint covers the bastion test route's not-found and
// persistence paths.
func TestSshBastionTestEndpoint(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLogin(t, ts.URL)

	now := time.Now().UTC().Format(time.RFC3339)
	// Names an auth key that resolves to nothing ⇒ cred_error offline, no dial.
	// (A keyless bastion would now take the TT reachability tier and dial.)
	if _, err := pool.Exec(`
		INSERT INTO bastions(id, hostname, name, address, port, username, auth_key_env_var, created_at)
		VALUES('b1','jump.example','jump','10.0.0.8',22,'jump','MISSING_KEY',?)`, now); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/bastions/b1/test", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /ssh/bastions/b1/test = %d, want 200", r.StatusCode)
	}
	var status string
	_ = pool.QueryRow(`SELECT status FROM bastions WHERE id='b1'`).Scan(&status)
	if status != "cred_error" {
		t.Fatalf("persisted bastion status = %q, want cred_error", status)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/bastions/ghost/test", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("POST unknown bastion = %d, want 404", r.StatusCode)
	}
}
