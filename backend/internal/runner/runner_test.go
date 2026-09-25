package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/redactdict"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// captureNotifier records RunEnded events synchronously for assertions.
type captureNotifier struct{ events []notify.RunEvent }

func (c *captureNotifier) RunEnded(ev notify.RunEvent) { c.events = append(c.events, ev) }

// AlertRaised satisfies the Notifier seam's SL half. The runner never raises
// one — SLA breaches and missed fires are the scheduler's to detect — so this
// records nothing and exists to keep the fake honest about the interface.
func (c *captureNotifier) AlertRaised(notify.AlertEvent) {}

// newTestService opens a temp DB, runs migrations, and returns a Service wired
// to it. Mirrors the pattern in internal/auth/auth_test.go.
func newTestService(t *testing.T) *Service {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "runner_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{RunnerBootstrapToken: "test-bootstrap-token"}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetLogDir(t.TempDir())
	return svc
}

// mustLogPath resolves the per-run log path or fails the test. logPath returns
// an error because a trace ID is an unvalidated path parameter (LU-12); every
// test that wants the path wants a real one, so the error is a test failure
// rather than something each call site re-handles.
//
// The empty entity code is the FLAT layout (LU-7): these fixtures seed runs
// without an entity_code, exactly like every run enqueued before migration 710,
// so they exercise the coexistence path rather than the foldered one. Tests that
// want a folder call logPath directly with a code.
func mustLogPath(t *testing.T, s *Service, traceID string) string {
	t.Helper()
	p, err := s.logPath("", traceID)
	if err != nil {
		t.Fatalf("logPath(%q): %v", traceID, err)
	}
	return p
}

// insertRunner inserts a runner row directly for testing.
func insertRunner(t *testing.T, svc *Service, id, name, status string, caps []string) {
	t.Helper()
	capsJSON, _ := json.Marshal(caps)
	ts := now()
	// protocol_version is the CURRENT protocol deliberately: a runners row only
	// exists because registration created it, and registration refuses anything
	// below the floor. Leaving it at migration 390's DEFAULT 1 made every
	// fixture look like a pre-handshake agent, which was invisible until the
	// floor became a poll-time check (DM-2) and then failed a dozen tests at
	// once. A fixture that wants a stale runner says so with setProtocol.
	_, err := svc.db.Exec(`
		INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
		VALUES (?, ?, ?, 'Linux', ?, 0, 5, '1.0', ?, ?, ?)`,
		id, name, status, string(capsJSON), runnerproto.ProtocolVersion, ts, ts)
	if err != nil {
		t.Fatalf("insertRunner: %v", err)
	}
}

// insertQueuedRun inserts a queued run for testing.
func insertQueuedRun(t *testing.T, svc *Service, traceID, jobName, runType, scope string) {
	t.Helper()
	ts := now()
	_, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES (?, ?, ?, ?, 'queued', 'test', 'manual', 'runner', ?)`,
		traceID, jobName, runType, scope, ts)
	if err != nil {
		t.Fatalf("insertQueuedRun: %v", err)
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestDrainTimeoutFiresNotifier verifies the drain-timeout terminal path goes
// through the same seam as finalizeRun: a run failed by drain timeout fires a
// notification (and, by the same call, the RunFinished metric). Regression guard
// for the bug where this path bypassed notifications/metrics.
func TestDrainTimeoutFiresNotifier(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cap := &captureNotifier{}
	svc.WithNotifier(cap)

	runnerID := "runner-drain"
	insertRunner(t, svc, runnerID, "drainer", "draining", []string{"bash"})
	// A run stuck in 'running' on this runner.
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, created_at, started_at)
		VALUES ('stuck-1', 'nightly', 'bash', 'prod', 'running', ?, 'test', 'manual', ?, ?)`,
		runnerID, ts, ts); err != nil {
		t.Fatalf("insert running run: %v", err)
	}

	svc.forceOfflineOnDrainTimeout(ctx, runnerID)

	if len(cap.events) != 1 {
		t.Fatalf("expected 1 RunEnded event, got %d", len(cap.events))
	}
	ev := cap.events[0]
	if ev.TraceID != "stuck-1" || ev.Status != "failure" || ev.JobName != "nightly" {
		t.Fatalf("unexpected run-end event: %+v", ev)
	}
	// And the run is marked failed with the drain reason.
	var status, reason string
	_ = svc.db.QueryRow(`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE id='stuck-1'`).Scan(&status, &reason)
	if status != "failure" || reason != "drain_timeout" {
		t.Fatalf("run not finalized: status=%q reason=%q", status, reason)
	}
}

// TestClaimRunTransition verifies the queued→running transition and that
// exactly one run is claimed per call (B4 seam test).
func TestClaimRunTransition(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	runnerID := "runner-1"
	insertRunner(t, svc, runnerID, "test-runner", "online", []string{"bash", "ansible"})

	traceA := db.NewTraceID()
	traceB := db.NewTraceID()
	insertQueuedRun(t, svc, traceA, "job-a", "bash", "prod")
	insertQueuedRun(t, svc, traceB, "job-b", "bash", "staging")

	// First claim: should get the oldest (traceA).
	got, err := svc.claimRun(ctx, runnerID, []string{"bash", "ansible"}, true)
	if err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	if got == nil {
		t.Fatal("expected a run assignment, got nil")
	}
	if got.TraceID != traceA {
		t.Errorf("expected traceA=%q, got %q", traceA, got.TraceID)
	}
	if got.RunType != "bash" {
		t.Errorf("expected runType=bash, got %q", got.RunType)
	}

	// Verify DB status changed.
	var status string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, traceA).Scan(&status)
	if status != "running" {
		t.Errorf("run status = %q, want running", status)
	}

	// Second claim: should get traceB.
	got2, err := svc.claimRun(ctx, runnerID, []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun 2: %v", err)
	}
	if got2 == nil || got2.TraceID != traceB {
		t.Errorf("expected traceB, got %v", got2)
	}

	// Third claim: no work.
	got3, err := svc.claimRun(ctx, runnerID, []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun 3: %v", err)
	}
	if got3 != nil {
		t.Errorf("expected nil, got %v", got3)
	}
}

// TestCapabilityGuard verifies that runs for types not in the runner's
// capabilities are not claimed (A6.3).
func TestCapabilityGuard(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	runnerID := "runner-bash-only"
	insertRunner(t, svc, runnerID, "bash-runner", "online", []string{"bash"})

	traceID := db.NewTraceID()
	insertQueuedRun(t, svc, traceID, "tf-job", "terraform", "prod")

	got, err := svc.claimRun(ctx, runnerID, []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	if got != nil {
		t.Errorf("bash runner should not claim terraform run, got %v", got)
	}

	// Run should still be queued.
	var status string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, traceID).Scan(&status)
	if status != "queued" {
		t.Errorf("run status = %q, want queued", status)
	}
}

// TestResumeOffset verifies the X-Resume-Offset handling in log ingest (T6).
func TestResumeOffset(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	// Insert a running run owned by a known runner (R1.4: ingest is authorized by
	// run ownership, so the request must be runner-authed and own the run).
	runnerID, tok := "runner-resume", "crn_run_resume"
	insertRunner(t, svc, runnerID, "resume", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "test-job", "bash", "", runnerID, "")

	logH := as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog))

	// Write some bytes to the log file AND seed log_raw_offset in the DB to
	// simulate a previous partial ingest (PP-M8: the server now validates
	// X-Resume-Offset against log_raw_offset rather than fi.Size()).
	existingContent := "line one\nline two\n"
	logPath := mustLogPath(t, svc, traceID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(existingContent), 0640); err != nil {
		t.Fatal(err)
	}
	persistedLen := int64(len(existingContent))
	if _, err := svc.db.Exec(`UPDATE runs SET log_raw_offset = ? WHERE id = ?`, persistedLen, traceID); err != nil {
		t.Fatalf("seed log_raw_offset: %v", err)
	}

	// Test 1: wrong resume offset → 409.
	{
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log",
			strings.NewReader("more data\n"))
		req.Header.Set("X-Resume-Offset", "999") // wrong
		req.Header.Set("Authorization", "Bearer "+tok)
		req.SetPathValue("traceId", traceID)

		rec := httptest.NewRecorder()
		logH.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Errorf("wrong offset: got %d, want 409; body: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		_ = json.NewDecoder(rec.Body).Decode(&body)
		if body["persistedOffset"] == nil {
			t.Error("409 body should include persistedOffset")
		}
	}

	// Test 2: correct resume offset → accepted.
	{
		envelope := `{"exitCode":0,"durationMs":100,"endedAt":"2026-06-09T12:00:00Z"}`
		body := envelope + "\n"
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log",
			strings.NewReader(body))
		req.Header.Set("X-Resume-Offset", fmt.Sprintf("%d", persistedLen))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.SetPathValue("traceId", traceID)

		rec := httptest.NewRecorder()
		logH.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("correct offset: got %d, want 204; body: %s", rec.Code, rec.Body.String())
		}
	}
}

// TestRunFinalization verifies that a complete log stream (with envelope)
// transitions the run to success and writes a run-end activity row.
func TestRunFinalization(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	runnerID, tok := "runner-final", "crn_run_final"
	insertRunner(t, svc, runnerID, "final", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID := db.NewTraceID()
	insertClaimedRun(t, svc, traceID, "my-job", "bash", "", runnerID, "")

	envelope := `{"exitCode":0,"durationMs":5000,"endedAt":"2026-06-09T14:00:00Z"}`
	body := "Hello world\n" + "Job done\n" + envelope + "\n"

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.SetPathValue("traceId", traceID)

	rec := httptest.NewRecorder()
	as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ingest: got %d want 204; body: %s", rec.Code, rec.Body.String())
	}

	// Verify run transitioned to success.
	var status string
	var exitCode int
	var durationMs int64
	if err := svc.db.QueryRow(
		`SELECT status, exit_code, duration_ms FROM runs WHERE id = ?`, traceID).
		Scan(&status, &exitCode, &durationMs); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if status != "success" {
		t.Errorf("run status = %q, want success", status)
	}
	if exitCode != 0 {
		t.Errorf("exit_code = %d, want 0", exitCode)
	}
	if durationMs != 5000 {
		t.Errorf("duration_ms = %d, want 5000", durationMs)
	}

	// Verify log file content.
	lines, err := linesTo(mustLogPath(t, svc, traceID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(lines) < 2 {
		t.Errorf("expected at least 2 log lines, got %d: %v", len(lines), lines)
	}

	// Verify activity row was written.
	var activityCount int
	_ = svc.db.QueryRow(
		`SELECT COUNT(1) FROM activity WHERE kind = 'run-end' AND trace_id = ?`, traceID).
		Scan(&activityCount)
	if activityCount != 1 {
		t.Errorf("expected 1 run-end activity, got %d", activityCount)
	}
}

// TestRedact verifies the redaction pass masks known sensitive values.
func TestRedact(t *testing.T) {
	r := &Redactor{dict: redactdict.FromValues([]string{"supersecret", "password123"})}

	cases := []struct {
		input string
		want  string
	}{
		{"no secret here", "no secret here"},
		{"value is supersecret!", "value is [REDACTED]!"},
		{"password123 in log", "[REDACTED] in log"},
		{"multi supersecret and password123", "multi [REDACTED] and [REDACTED]"},
	}
	for _, tc := range cases {
		got := string(r.Redact([]byte(tc.input)))
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestDrainTransition verifies that drain sets draining status and immediately
// goes to offline if load is zero.
func TestDrainTransition(t *testing.T) {
	svc := newTestService(t)

	runnerID := db.NewID()
	insertRunner(t, svc, runnerID, "drain-runner", "online", []string{"bash"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/"+runnerID+"/drain",
		strings.NewReader(`{"timeoutMinutes":30}`))
	req.SetPathValue("id", runnerID)

	rec := httptest.NewRecorder()
	svc.HandleDrain(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("drain: got %d want 202; body: %s", rec.Code, rec.Body.String())
	}

	var resp runnerResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// load is 0, so should immediately go offline.
	if resp.Status != "offline" {
		t.Errorf("status = %q, want offline (load=0)", resp.Status)
	}
}

// TestRegistrationToken verifies registration token checking (Phase 7:
// checkRegistrationToken distinguishes valid / unknown / DB-row tokens; the
// bootstrap token stays multi-use with no row).
func TestRegistrationToken(t *testing.T) {
	svc := newTestService(t)

	// Bootstrap token should validate, with no row id (never consumed).
	chk, err := svc.checkRegistrationToken(context.Background(), "test-bootstrap-token")
	if err != nil {
		t.Fatal(err)
	}
	if !chk.OK || chk.RowID != 0 {
		t.Errorf("bootstrap token: got %+v, want OK with RowID 0", chk)
	}

	// Unknown token should not validate.
	chk, err = svc.checkRegistrationToken(context.Background(), "bad-token")
	if err != nil {
		t.Fatal(err)
	}
	if chk.OK {
		t.Error("bad token should not be valid")
	}

	// Insert a token and verify it validates and carries its row id.
	ts := now()
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	token := "crn_reg_testtoken123"
	res, err := svc.db.Exec(`
		INSERT INTO registration_tokens(token_hash, created_by, created_at, expires_at)
		VALUES (?, 'admin', ?, ?)`, hashToken(token), ts, expiry)
	if err != nil {
		t.Fatalf("insert token: %v", err)
	}
	rowID, _ := res.LastInsertId()
	chk, err = svc.checkRegistrationToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if !chk.OK || chk.RowID != rowID {
		t.Errorf("inserted token: got %+v, want OK with RowID %d", chk, rowID)
	}
}

// TestRegisterProtocolVersionHandshake verifies the R0.2 version handshake:
// an agent declaring a protocol below MinProtocolVersion is rejected loudly
// (426), while the current protocol version is accepted (201).
func TestRegisterProtocolVersionHandshake(t *testing.T) {
	register := func(t *testing.T, svc *Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register",
			strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-bootstrap-token")
		rec := httptest.NewRecorder()
		svc.HandleRegisterRunner(rec, req)
		return rec
	}

	t.Run("too old rejected", func(t *testing.T) {
		svc := newTestService(t)
		rec := register(t, svc, fmt.Sprintf(`{"name":"old-agent","os":"Linux","capabilities":["bash"],"version":"1.0","protocolVersion":%d}`, runnerproto.MinProtocolVersion-1))
		if rec.Code != http.StatusUpgradeRequired {
			t.Fatalf("too-old protocol: got %d, want 426; body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "too old") {
			t.Errorf("expected an actionable 'too old' message, got: %s", rec.Body.String())
		}
		// No runner row should have been created.
		var n int
		_ = svc.db.QueryRow(`SELECT COUNT(1) FROM runners`).Scan(&n)
		if n != 0 {
			t.Errorf("rejected registration should not insert a runner row, got %d", n)
		}
	})

	t.Run("current accepted", func(t *testing.T) {
		svc := newTestService(t)
		rec := register(t, svc, fmt.Sprintf(`{"name":"new-agent","os":"Linux","capabilities":["bash"],"version":"1.0","protocolVersion":%d}`, runnerproto.ProtocolVersion))
		if rec.Code != http.StatusCreated {
			t.Fatalf("current protocol: got %d, want 201; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("absent rejected — counts as 0, below the floor", func(t *testing.T) {
		svc := newTestService(t)
		rec := register(t, svc, `{"name":"legacy-agent","os":"Linux","capabilities":["bash"],"version":"1.0"}`)
		if rec.Code != http.StatusUpgradeRequired {
			t.Fatalf("absent protocol: got %d, want 426 (no pre-handshake carve-out since the floor tracks the current protocol); body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestTrailingEnvelope verifies envelope parsing.
func TestTrailingEnvelope(t *testing.T) {
	cases := []struct {
		input    string
		wantNil  bool
		exitCode int
	}{
		{`{"exitCode":0,"durationMs":1000,"endedAt":"2026-06-09T12:00:00Z"}`, false, 0},
		{`{"exitCode":1,"durationMs":200,"endedAt":"2026-06-09T12:01:00Z"}`, false, 1},
		{`plain log line`, true, 0},
		{`{"no_endedAt": true}`, true, 0},
		{"", true, 0},
	}
	for _, tc := range cases {
		env := parseTrailingEnvelope(tc.input)
		if tc.wantNil {
			if env != nil {
				t.Errorf("parseTrailingEnvelope(%q) = %v, want nil", tc.input, env)
			}
		} else {
			if env == nil {
				t.Errorf("parseTrailingEnvelope(%q) = nil, want non-nil", tc.input)
			} else if env.ExitCode != tc.exitCode {
				t.Errorf("exitCode = %d, want %d", env.ExitCode, tc.exitCode)
			}
		}
	}
}

// TestRegisterStoresToolchains (Phase 4, RX.7): the agent's toolchains detail +
// widened capability tokens round-trip through registration into the runners row
// and back out on GET /runners.
func TestRegisterStoresToolchains(t *testing.T) {
	svc := newTestService(t)
	body := `{"name":"tc-agent","os":"Linux","version":"1.0","protocolVersion":` + strconv.Itoa(runnerproto.ProtocolVersion) + `,
		"capabilities":["ansible","checkout","vault","collection:community.vmware"],
		"toolchains":{"ansibleCore":"2.16.3","checkout":true,"vault":true,"collections":{"community.vmware":"3.5.0"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-bootstrap-token")
	rec := httptest.NewRecorder()
	svc.HandleRegisterRunner(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	// Persisted verbatim on the runners row.
	var tc string
	if err := svc.db.QueryRow(`SELECT toolchains FROM runners WHERE name='tc-agent'`).Scan(&tc); err != nil {
		t.Fatalf("read toolchains: %v", err)
	}
	if !strings.Contains(tc, `"ansibleCore":"2.16.3"`) || !strings.Contains(tc, "community.vmware") {
		t.Errorf("toolchains not stored verbatim: %s", tc)
	}

	// Surfaced on GET /runners.
	lrec := httptest.NewRecorder()
	svc.HandleListRunners(lrec, httptest.NewRequest(http.MethodGet, "/api/v1/runners", nil))
	if !strings.Contains(lrec.Body.String(), `"ansibleCore":"2.16.3"`) {
		t.Errorf("GET /runners should surface toolchains: %s", lrec.Body.String())
	}
	if !strings.Contains(lrec.Body.String(), `"collection:community.vmware"`) {
		t.Errorf("GET /runners should surface widened capability tokens: %s", lrec.Body.String())
	}
}
