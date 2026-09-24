package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/seed"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/web"
)

// newTestServer boots the full HTTP handler against a fresh migrated DB.
func newTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	return newTestServerWithLogDir(t, "")
}

// newTestServerWithLogDir is like newTestServer but, when logDir is non-empty,
// seeds log_storage_config.local_path BEFORE the handler is built. This makes
// both the runner's GET-log read path (LogDir resolved at mount) and the SSH
// test-run write path (resolved per-request) agree on a writable temp dir.
func newTestServerWithLogDir(t *testing.T, logDir string) (*httptest.Server, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if logDir != "" {
		if _, err := pool.Exec(
			`INSERT INTO log_storage_config(id, backend, local_path) VALUES(1, 'local', ?)`,
			logDir); err != nil {
			t.Fatalf("seed log dir: %v", err)
		}
	}

	cfg := &config.Config{
		Addr:                  ":0",
		CookieSecure:          false,
		RunnerBootstrapToken:  "boot-token",
		DevAuth:               true, // exercise the local dev-login bypass in tests
		SecretKEKEnv:          "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
		RunnerOfflineAfter:    5 * time.Minute,
		RunnerDeregisterAfter: 336 * time.Hour,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)

	srv := api.New(api.Options{
		Config:      cfg,
		Logger:      log,
		Auth:        authSvc,
		DB:          pool,
		ReadyChecks: []api.ReadyCheck{{Name: "database", Check: db.ReadyCheck(pool)}},
		WebFS:       web.DistFS(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, pool
}

func TestHealthAndReadiness(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestAuthGatingMatrix(t *testing.T) {
	ts, _ := newTestServer(t)

	// Operator routes require a session → 401 without one.
	operatorGets := []string{
		"/api/v1/me", "/api/v1/jobs", "/api/v1/workflows", "/api/v1/runs",
		"/api/v1/activity", "/api/v1/change-log", "/api/v1/env-vars",
		"/api/v1/scopes", "/api/v1/runners", "/api/v1/git/sync",
	}
	for _, p := range operatorGets {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401 (auth-gated)", p, resp.StatusCode)
		}
	}

	// Runner poll requires a bearer token → 401 without one.
	resp, err := http.Get(ts.URL + "/api/v1/runners/some-id/poll")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("runner poll without bearer = %d, want 401", resp.StatusCode)
	}
}

// TestDevLoginListsPopulatedData logs in via the dev bypass, then lists every
// view-backing endpoint against a seeded DB. It guards two things the empty-DB
// tests can't: that dev-login establishes a working session, and that the list
// handlers (which run a per-row follow-up query while the outer result set is
// still streaming) don't self-deadlock on the connection pool when rows exist.
func TestDevLoginListsPopulatedData(t *testing.T) {
	ts, pool := newTestServer(t)
	if err := seed.Seed(context.Background(), pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't follow the 302 to the SPA root
		},
	}

	resp, err := client.Get(ts.URL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("dev-login = %d, want 302", resp.StatusCode)
	}

	// /me confirms the session decodes to the synthetic developer.
	meResp, err := client.Get(ts.URL + "/api/v1/me")
	if err != nil || meResp.StatusCode != http.StatusOK {
		t.Fatalf("/me after dev-login = %v / %d", err, meResp.StatusCode)
	}
	meResp.Body.Close()

	// Every list endpoint must return 200 promptly (no deadlock) with data present.
	for _, p := range []string{
		"/api/v1/jobs", "/api/v1/runs", "/api/v1/workflows", "/api/v1/workflow-runs",
		"/api/v1/activity", "/api/v1/change-log", "/api/v1/runners", "/api/v1/scopes",
		"/api/v1/env-vars", "/api/v1/recent-logins",
	} {
		r, err := client.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v (deadlock?)", p, err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200; body=%s", p, r.StatusCode, body)
		}
		if len(body) < 3 { // at least "[]" / "{}"
			t.Errorf("GET %s returned empty body", p)
		}
	}

	// Guard the activity actor/outcome column order (they were once swapped in the
	// scan, mislabelling every row). outcome must be a valid enum or null; actor
	// must NOT be an outcome value.
	actResp, err := client.Get(ts.URL + "/api/v1/activity")
	if err != nil {
		t.Fatalf("GET /activity: %v", err)
	}
	var act struct {
		Items []struct {
			Actor   string `json:"actor"`
			Outcome string `json:"outcome"`
			Kind    string `json:"kind"`
		} `json:"items"`
	}
	json.NewDecoder(actResp.Body).Decode(&act)
	actResp.Body.Close()
	if len(act.Items) == 0 {
		t.Fatal("/activity returned no items")
	}
	validOutcome := map[string]bool{"": true, "success": true, "failure": true, "warning": true}
	for _, it := range act.Items {
		if !validOutcome[it.Outcome] {
			t.Errorf("activity outcome %q is not a valid enum (actor/outcome swapped?)", it.Outcome)
		}
		if it.Actor == "success" || it.Actor == "failure" || it.Actor == "warning" {
			t.Errorf("activity actor %q looks like an outcome value (columns swapped?)", it.Actor)
		}
	}
}

// TestRunnerRegisterAndPoll exercises the core DB-bus path end-to-end through
// the real HTTP handlers: a runner registers with the bootstrap token, a queued
// run is enqueued, and the runner's long-poll claims it (queued→running).
func TestRunnerRegisterAndPoll(t *testing.T) {
	ts, pool := newTestServer(t)

	// Register a bash-capable runner with the bootstrap token.
	body, _ := json.Marshal(map[string]any{
		"name": "runner-1", "os": "Linux", "capabilities": []string{"bash"}, "version": "1.0.0",
		"protocolVersion": runnerproto.ProtocolVersion,
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runners/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer boot-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, want 200/201", resp.StatusCode)
	}
	var reg struct {
		Runner struct {
			ID string `json:"id"`
		} `json:"runner"`
		APIKey string `json:"apiKey"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if reg.Runner.ID == "" || reg.APIKey == "" {
		t.Fatalf("register response missing runner.id/apiKey: %+v", reg)
	}

	// Enqueue a queued bash run for the runner to claim (the B5→B4 seam).
	traceID := db.NewTraceID()
	if _, err := pool.Exec(`
		INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, executor, created_at)
		VALUES (?, 'integration-job', 'bash', 'queued', 'tester', 'manual', 'runner', '2026-06-09T00:00:00Z')`,
		traceID); err != nil {
		t.Fatalf("seed queued run: %v", err)
	}

	// Poll — should immediately claim the queued run.
	preq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runners/"+reg.Runner.ID+"/poll", nil)
	preq.Header.Set("Authorization", "Bearer "+reg.APIKey)
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d, want 200 (work available)", presp.StatusCode)
	}
	raw, _ := io.ReadAll(presp.Body)
	if !strings.Contains(string(raw), traceID) {
		t.Fatalf("poll response did not contain the queued run %s: %s", traceID, raw)
	}
}

func TestSettingsEndpointsIntegration(t *testing.T) {
	ts, _ := newTestServer(t)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Dev login
	resp, err := client.Get(ts.URL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login: %v", err)
	}
	resp.Body.Close()

	// Extract CSRF token
	u, _ := url.Parse(ts.URL)
	var csrfToken string
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "amadeus_csrf" {
			csrfToken = cookie.Value
		}
	}

	// Secret-bearing settings need a KEK — newTestServer sets SecretKEKEnv.

	// 1. GitLab Settings GET / PUT
	var gitlabCfg settings.GitlabConfig
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/settings/gitlab", nil)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings/gitlab = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&gitlabCfg)
	r.Body.Close()

	gitlabCfg.BotName = "custom-bot-name"
	gitlabCfg.Pat = "new-pat-token"
	b, _ := json.Marshal(gitlabCfg)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/gitlab", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT /settings/gitlab = %d, body = %s", r.StatusCode, body)
	}
	r.Body.Close()

	// 2. Vault Settings GET / PUT
	var vaultCfg settings.VaultConfig
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/settings/vault", nil)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings/vault = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&vaultCfg)
	r.Body.Close()

	vaultCfg.Addr = "http://vault.prod:8200"
	vaultCfg.RoleId = "role-123"
	vaultCfg.SecretId = "secret-123"
	b, _ = json.Marshal(vaultCfg)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/vault", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT /settings/vault = %d, body = %s", r.StatusCode, body)
	}
	r.Body.Close()

	// 3. Log Storage Settings GET / PUT
	var logStorageCfg settings.LogStorageConfig
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/settings/log-storage", nil)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings/log-storage = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&logStorageCfg)
	r.Body.Close()

	logStorageCfg.Local = &settings.LocalLogConfig{Path: "/var/lib/amadeus/it-logs"}
	b, _ = json.Marshal(logStorageCfg)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/log-storage", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT /settings/log-storage = %d, body = %s", r.StatusCode, body)
	}
	r.Body.Close()

	// SL-1: backend=s3 is no longer refused outright — the save probes the
	// bucket. An unreachable endpoint is a 422 with the probe error, and the row
	// is left as the previous save wrote it (refuse-don't-persist).
	s3Cfg := settings.LogStorageConfig{
		Backend: "s3",
		Local:   &settings.LocalLogConfig{Path: "/var/lib/amadeus/it-logs"},
		S3: &settings.S3LogConfig{
			Endpoint: "127.0.0.1:1", Bucket: "logs", Region: "us-east-1",
			AccessKey: "AKIA", SecretKey: "secret", Prefix: "amadeus/",
		},
	}
	b, _ = json.Marshal(s3Cfg)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/log-storage", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body422, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT /settings/log-storage backend=s3 unreachable = %d, body = %s", r.StatusCode, body422)
	}
	if !bytes.Contains(body422, []byte("probe failed")) {
		t.Fatalf("422 body should carry the probe error: %s", body422)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/settings/log-storage", nil)
	r, _ = client.Do(req)
	var after settings.LogStorageConfig
	json.NewDecoder(r.Body).Decode(&after)
	r.Body.Close()
	if after.Backend != "local" || after.Local == nil || after.Local.Path != "/var/lib/amadeus/it-logs" || after.S3 != nil {
		t.Fatalf("refused s3 save changed the row: %+v", after)
	}

	// 4. Observability Settings GET / PUT
	var obsCfg settings.ObservabilityConfig
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/settings/observability", nil)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings/observability = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&obsCfg)
	r.Body.Close()

	obsCfg.Path = "/metrics-endpoint"
	obsCfg.AuthType = "bearer"
	obsCfg.BearerToken = "tok-123"
	b, _ = json.Marshal(obsCfg)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/observability", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT /settings/observability = %d, body = %s", r.StatusCode, body)
	}
	r.Body.Close()

	// The metrics guard applies live (E.5): bearer auth was just enabled, so an
	// unauthenticated scrape is rejected and the configured token is accepted.
	// (The custom path needs a restart; the startup mount at /metrics still serves.)
	r, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /metrics without bearer = %d, want 401", r.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer tok-123")
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics with bearer = %d, want 200", r.StatusCode)
	}

	// 5. GitLab Webhook Secret Rotation POST
	rotInput := map[string]any{
		"updateGitlab":   false,
		"overlapMinutes": 10,
	}
	b, _ = json.Marshal(rotInput)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/settings/gitlab/webhook-secret/rotate", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("POST /settings/gitlab/webhook-secret/rotate = %d, body = %s", r.StatusCode, body)
	}
	var rotResult map[string]any
	json.NewDecoder(r.Body).Decode(&rotResult)
	r.Body.Close()

	if rotResult["secret"] == nil || rotResult["overlapUntil"] == nil {
		t.Fatalf("unexpected rotation response: %+v", rotResult)
	}
}

// devLoginClient boots a session-authenticated HTTP client via the dev-login
// bypass (newTestServer sets DevAuth=true).
func devLoginClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("dev-login = %d, want 302", resp.StatusCode)
	}
	return client
}

// TestSchedulesAndJobDetail exercises the multi-schedule API surface: the
// inventory + upcoming projection endpoints, and the Gap B job-detail field set
// (the fields the builder must echo back to avoid silent data loss on publish).
func TestSchedulesAndJobDetail(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs(name, run_type, enabled, concurrency_policy, concurrency_key,
		                 timeout_seconds, retries, command, executor, synced_at)
		VALUES('sched-test','bash',1,'Forbid','sched-test',120,2,'echo hi','ssh','2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_kind, owner_name, name, cron, env, position)
		VALUES('job','sched-test','nightly','0 0 * * *','{"STAGE":"prod"}',0)
	`); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name='sched-test'`).Scan(&rowid); err != nil {
		t.Fatalf("lookup rowid: %v", err)
	}

	client := devLoginClient(t, ts)

	// ── Inventory ──────────────────────────────────────────────────────────────
	var inv struct {
		Items []struct {
			OwnerKind    string            `json:"ownerKind"`
			OwnerName    string            `json:"ownerName"`
			ScheduleName string            `json:"scheduleName"`
			Cron         string            `json:"cron"`
			Env          map[string]string `json:"env"`
			Enabled      bool              `json:"enabled"`
			Paused       bool              `json:"paused"`
			NextRunAt    *string           `json:"nextRunAt"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedules", &inv)
	var found bool
	for _, it := range inv.Items {
		if it.OwnerKind == "job" && it.OwnerName == "sched-test" && it.ScheduleName == "nightly" {
			found = true
			if !it.Enabled || it.Paused {
				t.Errorf("entry enabled/paused = %v/%v, want true/false", it.Enabled, it.Paused)
			}
			if it.NextRunAt == nil {
				t.Error("enabled entry should have a nextRunAt")
			}
			if it.Env["STAGE"] != "prod" {
				t.Errorf("env STAGE = %q, want prod", it.Env["STAGE"])
			}
		}
	}
	if !found {
		t.Fatalf("inventory missing sched-test/nightly: %+v", inv.Items)
	}

	// ── Upcoming (7d window → ~7 daily fires, ascending) ──────────────────────
	var up struct {
		Window string `json:"window"`
		Items  []struct {
			OwnerName string `json:"ownerName"`
			At        string `json:"at"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedules/upcoming?window=7d", &up)
	if up.Window != "7d" {
		t.Errorf("window = %q, want 7d", up.Window)
	}
	var count int
	var prev string
	for _, it := range up.Items {
		if it.OwnerName != "sched-test" {
			continue
		}
		count++
		if prev != "" && it.At < prev {
			t.Errorf("upcoming not ascending: %s before %s", prev, it.At)
		}
		prev = it.At
	}
	if count < 5 || count > 8 {
		t.Errorf("7d daily projection produced %d fires, want ~7", count)
	}

	// ── Job detail: Gap B field set + schedules round-trip ────────────────────
	var job struct {
		Command           *string `json:"command"`
		Executor          *string `json:"executor"`
		ConcurrencyPolicy string  `json:"concurrencyPolicy"`
		ConcurrencyKey    *string `json:"concurrencyKey"`
		TimeoutSeconds    *int64  `json:"timeoutSeconds"`
		Retries           int     `json:"retries"`
		NextRunAt         *string `json:"nextRunAt"`
		Schedules         []struct {
			Name      string  `json:"name"`
			Cron      string  `json:"cron"`
			NextRunAt *string `json:"nextRunAt"`
		} `json:"schedules"`
	}
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+itoa(rowid), &job)
	if job.Command == nil || *job.Command != "echo hi" {
		t.Errorf("command = %v, want 'echo hi'", job.Command)
	}
	if job.ConcurrencyPolicy != "Forbid" {
		t.Errorf("concurrencyPolicy = %q, want Forbid", job.ConcurrencyPolicy)
	}
	if job.TimeoutSeconds == nil || *job.TimeoutSeconds != 120 {
		t.Errorf("timeoutSeconds = %v, want 120", job.TimeoutSeconds)
	}
	if job.Retries != 2 {
		t.Errorf("retries = %d, want 2", job.Retries)
	}
	if len(job.Schedules) != 1 || job.Schedules[0].Name != "nightly" {
		t.Fatalf("schedules = %+v, want one 'nightly' entry", job.Schedules)
	}
	if job.Schedules[0].NextRunAt == nil || job.NextRunAt == nil {
		t.Error("expected non-nil nextRunAt on entry and definition")
	}
}

// devLoginWithCSRF logs in via the dev bypass and returns the session client plus
// its CSRF token, for exercising state-changing endpoints.
func devLoginWithCSRF(t *testing.T, ts *httptest.Server) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login: %v", err)
	}
	resp.Body.Close()
	u, _ := url.Parse(ts.URL)
	var csrf string
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "amadeus_csrf" {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF token after dev-login")
	}
	return client, csrf
}

// TestAccessEndpointsIntegration exercises the LB3 Users & Roles surface end-to-end:
// the role list, the AD-group→role mapping CRUD (with the CSRF guard), and the
// scope-restriction matrix round-trip. These four operations were spec'd, resolved
// against at login, and wired in the SPA — but had no handlers until LB3.
func TestAccessEndpointsIntegration(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	// ── Roles ────────────────────────────────────────────────────────────────
	var roles []struct {
		Name string `json:"name"`
	}
	getJSON(t, client, ts.URL+"/api/v1/roles", &roles)
	if len(roles) != 4 {
		t.Fatalf("roles = %+v, want 4 built-ins", roles)
	}

	// ── Access-grant CRUD ────────────────────────────────────────────────────
	// This exercised /ad-group-mappings and /scope-restrictions until v0.57.8 (RB-19)
	// retired both. /access-grants is the surface that replaced them AND the one that
	// actually decides access, so the end-to-end round-trip belongs here now.

	// Forged POST (no CSRF header) is rejected.
	b, _ := json.Marshal(map[string]any{"adGroup": "SG-Forged", "role": "Operator", "allScopes": true})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/access-grants", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", r.StatusCode)
	}

	// Create (role given Capitalized as the SPA sends it → stored canonical lowercase).
	b, _ = json.Marshal(map[string]any{"adGroup": "SG-Cronomicon-Ops", "role": "Operator", "allScopes": true})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/access-grants", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated || created.ID == "" {
		t.Fatalf("create grant = %d id=%q", r.StatusCode, created.ID)
	}
	if created.Role != "operator" {
		t.Fatalf("role = %q, want canonical lowercase 'operator'", created.Role)
	}

	// List returns it.
	var list []struct {
		ID, AdGroup, Role string
	}
	getJSON(t, client, ts.URL+"/api/v1/access-grants", &list)
	if len(list) != 1 || list[0].AdGroup != "SG-Cronomicon-Ops" {
		t.Fatalf("list = %+v, want one SG-Cronomicon-Ops grant", list)
	}

	// Update the role.
	b, _ = json.Marshal(map[string]any{"adGroup": "SG-Cronomicon-Ops", "role": "Viewer", "allScopes": true})
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/access-grants/"+created.ID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var updated struct {
		Role string `json:"role"`
	}
	json.NewDecoder(r.Body).Decode(&updated)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || updated.Role != "viewer" {
		t.Fatalf("update grant = %d role=%q, want 200/viewer", r.StatusCode, updated.Role)
	}

	// Delete.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/access-grants/"+created.ID, nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete grant = %d, want 204", r.StatusCode)
	}
	getJSON(t, client, ts.URL+"/api/v1/access-grants", &list)
	if len(list) != 0 {
		t.Fatalf("list after delete = %+v, want empty", list)
	}
}

// getJSON GETs url and decodes a 200 JSON body into out.
func getJSON(t *testing.T, client *http.Client, url string, out any) {
	t.Helper()
	r, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("GET %s = %d, want 200; body=%s", url, r.StatusCode, body)
	}
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// seedJob inserts a minimal jobs row (R4/R5 trigger tests) and returns its rowid.
// executor may be "" (NULL ⇒ no spec.executor).
func seedJob(t *testing.T, pool *sql.DB, name, runType, executor string) int64 {
	t.Helper()
	var exec any
	if executor != "" {
		exec = executor
	}
	if _, err := pool.Exec(`
		INSERT INTO jobs(name, run_type, enabled, concurrency_policy, concurrency_key,
		                 command, executor, synced_at)
		VALUES(?, ?, 1, 'Allow', ?, 'echo hi', ?, '2026-01-01T00:00:00Z')
	`, name, runType, name, exec); err != nil {
		t.Fatalf("seed job %s: %v", name, err)
	}
	var rowid int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name = ?`, name).Scan(&rowid); err != nil {
		t.Fatalf("lookup rowid %s: %v", name, err)
	}
	return rowid
}

// triggerRun POSTs /jobs/{rowid}/run with an optional JSON body and returns the
// status code and decoded Run response (when 202).
func triggerRun(t *testing.T, client *http.Client, csrf, baseURL string, rowid int64, body map[string]any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/jobs/"+itoa(rowid)+"/run", rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	r, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer r.Body.Close()
	var out map[string]any
	if r.StatusCode == http.StatusAccepted {
		json.NewDecoder(r.Body).Decode(&out)
	}
	return r.StatusCode, out
}

// TestRunTriggerExecutorRouting covers R4.3 (ansible/terraform route to the
// runner executor and sit queued instead of 422) and the bash ssh default.
func TestRunTriggerExecutorRouting(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	// bash → ssh (capability default).
	bashID := seedJob(t, pool, "bash-job", "bash", "")
	code, run := triggerRun(t, client, csrf, ts.URL, bashID, nil)
	if code != http.StatusAccepted {
		t.Fatalf("bash trigger = %d, want 202", code)
	}
	if run["executor"] != "ssh" {
		t.Errorf("bash executor = %v, want ssh", run["executor"])
	}

	// ansible → runner, queued (NOT 422), with no capable runner registered.
	ansID := seedJob(t, pool, "ansible-job", "ansible", "")
	code, run = triggerRun(t, client, csrf, ts.URL, ansID, nil)
	if code != http.StatusAccepted {
		t.Fatalf("ansible trigger = %d, want 202 (queued for runner, not 422)", code)
	}
	if run["executor"] != "runner" {
		t.Errorf("ansible executor = %v, want runner", run["executor"])
	}
	if run["status"] != "queued" {
		t.Errorf("ansible status = %v, want queued", run["status"])
	}

	// terraform → runner, queued.
	tfID := seedJob(t, pool, "tf-job", "terraform", "")
	code, run = triggerRun(t, client, csrf, ts.URL, tfID, nil)
	if code != http.StatusAccepted {
		t.Fatalf("terraform trigger = %d, want 202", code)
	}
	if run["executor"] != "runner" {
		t.Errorf("terraform executor = %v, want runner", run["executor"])
	}
	if run["status"] != "queued" {
		t.Errorf("terraform status = %v, want queued", run["status"])
	}

	// Confirm the ansible run row is frozen executor='runner', status='queued'.
	var ex, st string
	if err := pool.QueryRow(`SELECT executor, status FROM runs WHERE job_name='ansible-job'`).Scan(&ex, &st); err != nil {
		t.Fatalf("read ansible run row: %v", err)
	}
	if ex != "runner" || st != "queued" {
		t.Errorf("ansible run row executor/status = %q/%q, want runner/queued", ex, st)
	}
}

// TestRunTriggerExecutorPrecedence proves the R5.1 precedence chain:
// per-trigger override > job spec.executor > global default > capability.
func TestRunTriggerExecutorPrecedence(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	ctx := context.Background()

	// Global default = runner. A bash job whose capability default is ssh should
	// pick up the global runner default (global beats capability).
	if _, err := settings.UpdateGlobalSettings(ctx, pool,
		settings.GlobalSettings{DefaultExecutor: "runner"}, "tester"); err != nil {
		t.Fatalf("set global default: %v", err)
	}
	gID := seedJob(t, pool, "global-bash", "bash", "")
	_, run := triggerRun(t, client, csrf, ts.URL, gID, nil)
	if run["executor"] != "runner" {
		t.Errorf("global default: executor = %v, want runner (global beats capability)", run["executor"])
	}

	// Job spec.executor = ssh beats the global runner default.
	sID := seedJob(t, pool, "spec-ssh", "bash", "ssh")
	_, run = triggerRun(t, client, csrf, ts.URL, sID, nil)
	if run["executor"] != "ssh" {
		t.Errorf("spec.executor: executor = %v, want ssh (spec beats global)", run["executor"])
	}

	// Per-trigger override = runner beats job spec.executor = ssh.
	_, run = triggerRun(t, client, csrf, ts.URL, sID, map[string]any{"executor": "runner"})
	if run["executor"] != "runner" {
		t.Errorf("override: executor = %v, want runner (override beats spec)", run["executor"])
	}
}

// TestRunTriggerInvalidExecutorCombo proves the R5.2 capability matrix: ssh +
// ansible/terraform is rejected with a clear 422.
func TestRunTriggerInvalidExecutorCombo(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	// Per-trigger override ssh on an ansible job → 422.
	ansID := seedJob(t, pool, "ansible-job", "ansible", "")
	code, _ := triggerRun(t, client, csrf, ts.URL, ansID, map[string]any{"executor": "ssh"})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("ssh+ansible = %d, want 422", code)
	}

	// Job spec.executor=ssh on a terraform job → 422.
	tfID := seedJob(t, pool, "tf-job", "terraform", "ssh")
	code, _ = triggerRun(t, client, csrf, ts.URL, tfID, nil)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("spec ssh+terraform = %d, want 422", code)
	}

	// An unknown executor value → 422.
	bashID := seedJob(t, pool, "bash-job", "bash", "")
	code, _ = triggerRun(t, client, csrf, ts.URL, bashID, map[string]any{"executor": "bogus"})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("bogus executor = %d, want 422", code)
	}
}

// TestResyncScopes verifies the POST /api/v1/scopes/resync endpoint's basic
// functionality and error response when git sync is unconfigured.
func TestResyncScopes(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	// Since GitLab isn't configured in the test server, sync will fail.
	// But it should still return Status 200 OK with the error listed in the "errors" field.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/scopes/resync", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /scopes/resync: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /scopes/resync status = %d, want 200", resp.StatusCode)
	}

	var res struct {
		ScopesSynced int `json:"scopesSynced"`
		Errors       []struct {
			File    string `json:"file"`
			Line    int    `json:"line"`
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
		Deltas []struct {
			Scope  string `json:"scope"`
			Change string `json:"change"`
		} `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(res.Errors) == 0 {
		t.Error("expected errors due to unconfigured gitlab, got none")
	}
	foundUnreachable := false
	for _, e := range res.Errors {
		if strings.Contains(strings.ToLower(e.Message), "gitlab") || strings.Contains(strings.ToLower(e.Message), "clone") || strings.Contains(strings.ToLower(e.Message), "unreachable") {
			foundUnreachable = true
		}
	}
	if !foundUnreachable {
		t.Errorf("expected error message to mention gitlab or unreachable, got: %+v", res.Errors)
	}
}

// TestRequestBodySizeLimit proves PP-M1: an oversized JSON body on a write route
// is rejected (rather than buffered whole and risking OOM on the single-process
// backend), while a normal-sized body on the same route still succeeds.
func TestRequestBodySizeLimit(t *testing.T) {
	ts, _ := newTestServer(t)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/api/v1/auth/dev-login")
	if err != nil {
		t.Fatalf("dev-login: %v", err)
	}
	resp.Body.Close()
	u, _ := url.Parse(ts.URL)
	var csrfToken string
	for _, c := range jar.Cookies(u) {
		if c.Name == "amadeus_csrf" {
			csrfToken = c.Value
		}
	}

	put := func(body []byte) int {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/gitlab", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrfToken)
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	// Oversized: a >2 MiB bot_name value. The cap stops the read mid-decode, so the
	// handler can never see (let alone buffer) the full payload — status must NOT be 200.
	huge := strings.Repeat("A", 3<<20)
	big, _ := json.Marshal(map[string]string{"bot_name": huge})
	if got := put(big); got == http.StatusOK || got < 400 {
		t.Fatalf("oversized PUT = %d, want a 4xx rejection (body cap not enforced)", got)
	}

	// Normal-sized body on the same route still works.
	small, _ := json.Marshal(map[string]string{"bot_name": "ci-bot"})
	if got := put(small); got != http.StatusOK {
		t.Fatalf("normal PUT = %d, want 200 (cap rejected a legitimate body)", got)
	}
}
