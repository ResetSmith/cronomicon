// Why this file exists (LU-5).
//
// Before this change the run-log directory was resolved on three different
// schedules: the runner service and the SSH executor each read it once at mount,
// while writeSSHTestLog re-read it from the DB on every request. Saving a new
// path in the settings panel therefore updated the DB and one of the three
// writers, and the handler papered over it with a Warn saying the change "takes
// effect on restart" — so an operator who moved logs off a filling volume watched
// runs keep landing on the old one, with the UI showing the new path.
//
// The property under test is that a single save now re-points every writer in
// process. That is a wiring property: it can only break by someone dropping a
// hook or a retained service, and it breaks silently — the endpoint still
// returns 200 and the settings page still shows the new value. So each writer is
// asserted individually rather than through a single aggregate, and a run log is
// driven end-to-end through the real ingest handler afterwards to prove the
// re-point reaches the bytes and not just the accessor.
package api

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// logDirSpy captures invocations of the LogDirChanged hook. Modelled on
// tzReloadSpy in timezone_test.go: the hook exists for the one consumer the
// Server cannot reach itself (the process-log sink owned by main), so the only
// way to prove it is wired is to observe the call.
type logDirSpy struct {
	mu    sync.Mutex
	calls []string
}

func (s *logDirSpy) hook(_ context.Context, dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, dir)
}

func (s *logDirSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// newLogDirTestServer builds a fully-mounted Server (mountRunners included, so
// runnerSvc/sshExec are actually populated) plus the spy and a captured logger.
//
// It deliberately does NOT reuse newTestServerWithLogDir from integration_test.go:
// that helper lives in package api_test and hands back only the httptest server,
// whereas every assertion here is on unexported state (logDirValue, runnerSvc,
// sshExec), which is only reachable from inside package api.
func newLogDirTestServer(t *testing.T, seedLogDir string) (*Server, *httptest.Server, *sql.DB, *logDirSpy, func(*testing.T) []capturedLog) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "logdir.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if seedLogDir != "" {
		if _, err := pool.Exec(
			`INSERT INTO log_storage_config(id, backend, local_path) VALUES(1, 'local', ?)`,
			seedLogDir); err != nil {
			t.Fatalf("seed log dir: %v", err)
		}
	}

	cfg := &config.Config{
		Addr:                 ":0",
		CookieSecure:         false,
		RunnerBootstrapToken: "boot-token",
		DevAuth:              true,
		SecretKEKEnv:         "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
		RunnerOfflineAfter:   5 * time.Minute,
	}
	log, records := newCapturingLogger(slog.LevelInfo)
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	spy := &logDirSpy{}

	srv := New(Options{
		Config:        cfg,
		Logger:        log,
		Auth:          authSvc,
		DB:            pool,
		ReadyChecks:   []ReadyCheck{{Name: "database", Check: db.ReadyCheck(pool)}},
		LogDirChanged: spy.hook,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, pool, spy, records
}

// logDirLogin performs the dev-login bypass and returns a cookie-jar client plus
// the CSRF token the settings PUT requires. Mirrors devLoginWithCSRF in
// integration_test.go, which is not visible from package api.
func logDirLogin(t *testing.T, ts *httptest.Server) (*http.Client, string) {
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
	for _, c := range jar.Cookies(u) {
		if c.Name == "cronomicon_csrf" {
			return client, c.Value
		}
	}
	t.Fatal("no CSRF token after dev-login")
	return nil, ""
}

// putLogDir saves a new local log path through the real endpoint.
func putLogDir(t *testing.T, client *http.Client, ts *httptest.Server, csrf, dir string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"backend": "local",
		"local":   map[string]string{"path": dir},
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/log-storage", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT /settings/log-storage: %v", err)
	}
	return resp
}

// TestSaveLogStorageRepointsEveryWriterInProcess is the core LU-5 assertion:
// after one save, the Server's cached value, the runner service and the SSH
// executor all agree on the new directory — no restart, and no writer left on the
// old path. Each is checked separately because they are three independent wirings
// in applyLogDir and any one of them could be dropped without the others noticing.
func TestSaveLogStorageRepointsEveryWriterInProcess(t *testing.T) {
	oldDir := t.TempDir()
	srv, ts, _, _, _ := newLogDirTestServer(t, oldDir)
	client, csrf := logDirLogin(t, ts)

	ctx := context.Background()
	if got := srv.logDirValue(ctx); got != oldDir {
		t.Fatalf("pre-change logDirValue = %q, want the seeded %q", got, oldDir)
	}
	if srv.runnerSvc == nil || srv.sshExec == nil {
		t.Fatal("mountRunners did not retain the runner service / SSH executor; applyLogDir has nothing to re-point")
	}

	newDir := t.TempDir()
	resp := putLogDir(t, client, ts, csrf, newDir)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body: %s", resp.StatusCode, body)
	}

	if got := srv.logDirValue(ctx); got != newDir {
		t.Errorf("logDirValue = %q, want %q — the per-request SSH test-log writer is still on the old path", got, newDir)
	}
	if got := srv.runnerSvc.LogDir(); got != newDir {
		t.Errorf("runnerSvc.LogDir() = %q, want %q — runner logs would still land in the old directory", got, newDir)
	}
	if got := srv.sshExec.LogDir(); got != newDir {
		t.Errorf("sshExec.LogDir() = %q, want %q — in-app SSH runs would still land in the old directory", got, newDir)
	}
}

// TestSaveLogStorageFiresLogDirChangedHook covers the consumer the Server cannot
// reach on its own: the process-log sink lives in main, and when its file is
// derived from the run-log directory it has to move with it. The hook is the only
// channel for that, and a Server built without it must still work — so the
// no-hook case is asserted too.
func TestSaveLogStorageFiresLogDirChangedHook(t *testing.T) {
	srv, ts, _, spy, _ := newLogDirTestServer(t, t.TempDir())
	client, csrf := logDirLogin(t, ts)

	if calls := spy.snapshot(); len(calls) != 0 {
		t.Fatalf("hook fired %v before any save", calls)
	}

	newDir := t.TempDir()
	resp := putLogDir(t, client, ts, csrf, newDir)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", resp.StatusCode)
	}

	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("LogDirChanged fired %d times, want exactly 1: %v", len(calls), calls)
	}
	if calls[0] != newDir {
		t.Errorf("LogDirChanged got %q, want the newly saved %q", calls[0], newDir)
	}
	// And the hook fired only after the save succeeded, so the process log can
	// never be pointed somewhere the stored settings disagree with.
	if got := srv.logDirValue(context.Background()); got != calls[0] {
		t.Errorf("hook argument %q disagrees with the server's own value %q", calls[0], got)
	}
}

// TestSaveLogStorageNoLongerWarnsAboutRestart pins the removal of the old
// behaviour, not just the addition of the new one. The Warn line was the
// operator-facing symptom of the bug — it told them to restart to get a change
// that had in fact only half-applied — and leaving it in place after the fix
// would keep producing unnecessary restarts of a running orchestrator.
func TestSaveLogStorageNoLongerWarnsAboutRestart(t *testing.T) {
	_, ts, _, _, records := newLogDirTestServer(t, t.TempDir())
	client, csrf := logDirLogin(t, ts)

	newDir := t.TempDir()
	resp := putLogDir(t, client, ts, csrf, newDir)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body: %s", resp.StatusCode, body)
	}
	// Scan the response for a restart notice, ignoring the echoed path itself —
	// t.TempDir() derives its name from the test function, which contains the
	// very word being searched for.
	scanned := strings.ReplaceAll(string(body), newDir, "<path>")
	if strings.Contains(strings.ToLower(scanned), "restart") {
		t.Errorf("response body mentions a restart: %s", scanned)
	}

	var sawSaveLine bool
	for _, rec := range records(t) {
		if !strings.Contains(rec.Msg, "log storage settings updated") {
			continue
		}
		sawSaveLine = true
		if strings.Contains(strings.ToLower(rec.Msg), "restart") {
			t.Errorf("log storage save still claims a restart is needed: %q", rec.Msg)
		}
		if rec.Level == slog.LevelWarn.String() {
			t.Errorf("log storage save logged at WARN (%q); a change that applies immediately is not a warning", rec.Msg)
		}
		if dir, _ := rec.Attrs["log_dir"].(string); dir != newDir {
			t.Errorf("save line log_dir = %q, want %q", dir, newDir)
		}
	}
	if !sawSaveLine {
		t.Error("no 'log storage settings updated' line was logged for the save")
	}
}

// TestRunLogAfterRepointLandsInTheNewDirectory is the end-to-end half. The
// accessor assertions above would all still pass if the runner service read its
// directory once per process somewhere below LogDir(); only driving real bytes
// through the real ingest handler proves an operator moving the log volume
// actually gets the next run's output on the new volume.
func TestRunLogAfterRepointLandsInTheNewDirectory(t *testing.T) {
	oldDir := t.TempDir()
	srv, ts, pool, _, _ := newLogDirTestServer(t, oldDir)
	client, csrf := logDirLogin(t, ts)

	newDir := t.TempDir()
	resp := putLogDir(t, client, ts, csrf, newDir)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", resp.StatusCode)
	}

	// A runner with a bound token, and a run it owns and is executing.
	runnerID, token := "runner-repoint", "crn_run_repoint"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
		VALUES (?, 'repoint', 'online', 'Linux', '["bash"]', 0, 5, '1.0', ?, ?)`,
		runnerID, now, now); err != nil {
		t.Fatalf("insert runner: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runner_tokens(token_hash, runner_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		auth.HashToken(token), runnerID, "runner:"+runnerID, now,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("insert runner token: %v", err)
	}
	traceID := db.NewTraceID()
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, executor,
		                 triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, 'repoint-job', 'bash', '', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
		traceID, runnerID, now, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/"+traceID+"/log",
		strings.NewReader("output after the re-point\n"))
	req.Header.Set("Authorization", "Bearer "+token)
	ingestResp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST run log: %v", err)
	}
	ingestBody, _ := io.ReadAll(ingestResp.Body)
	ingestResp.Body.Close()
	if ingestResp.StatusCode != http.StatusNoContent {
		t.Fatalf("ingest = %d, want 204; body: %s", ingestResp.StatusCode, ingestBody)
	}

	data, err := os.ReadFile(filepath.Join(newDir, traceID+".log"))
	if err != nil {
		t.Fatalf("run log is not in the new directory %s: %v", newDir, err)
	}
	if !strings.Contains(string(data), "output after the re-point") {
		t.Errorf("new-directory log = %q, want the ingested output", data)
	}
	if _, err := os.Stat(filepath.Join(oldDir, traceID+".log")); err == nil {
		t.Errorf("the run also wrote into the OLD directory %s — the re-point did not take", oldDir)
	}
	// The Server's own view agrees, so the read path resolves the same file.
	if got := srv.logDirValue(context.Background()); got != newDir {
		t.Errorf("logDirValue = %q, want %q", got, newDir)
	}
}
