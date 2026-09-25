package runner

// End-to-end integration test for R8.2: the REAL runner agent
// (internal/agent + the lifecycle it drives) against the REAL server runner
// handlers (this package) wired to a migrated SQLite DB and the real
// auth.RequireRunner middleware, executing over a real in-test SSH server.
//
// Home rationale (import cycle): internal/agent imports only runnerproto, so
// it never imports this package. A *_test.go in package runner may therefore
// import internal/agent with no production import cycle, and it gets direct
// access to this package's unexported seed helpers (newTestService,
// insertJobDef, authSvc, …) and the real handlers under test. That is why the
// E2E lives here rather than in internal/api (which would pull in the whole
// Server and a second migrated-DB helper for no benefit).
//
// What the happy path asserts end-to-end, against the real wire:
//   - registration persists a runners row and returns a usable apiKey;
//   - the agent's poll CLAIMS the seeded queued run (queued→running, runner_id set);
//   - the agent fetches the ownership-authorized manifest and executes over SSH;
//   - logs are ingested (per-run file written by the server; plain Variables
//     log-visible per D7) and the run reaches a terminal status via the
//     trailing envelope.
//
// Sub-cases (kill / drain) are added as separate top-level tests that reuse the
// same harness. Log-resume is covered deterministically by the unit test
// TestResumeOffset (this file) and internal/agent/logstream_test.go; an
// end-to-end dropped-connection variant would race the agent's internal stream
// loop, so we rely on those rather than ship a flaky test (see note at bottom).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agent"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"golang.org/x/crypto/ssh"
)

// ── In-test SSH target ─────────────────────────────────────────────────────────

// e2eSSHServer stands up a minimal x/crypto/ssh server that accepts the given
// client public key and, on exec, runs a tiny canned response then exits 0.
// Modeled on internal/agent/integration_test.go and internal/sshexec tests.
//
// stdoutLine is echoed verbatim on the channel before exit — the test seeds it
// with a known sensitive value so we can assert the SERVER redacts it on ingest.
func e2eSSHServer(t *testing.T, clientPub ssh.PublicKey, stdoutLine string) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go e2eServeSSHConn(conn, cfg, stdoutLine)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func e2eServeSSHConn(nConn net.Conn, cfg *ssh.ServerConfig, stdoutLine string) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					_ = req.Reply(true, nil)
					// DRAIN STDIN before finishing. Since H1 the agent invokes the
					// interpreter as `bash -s`, which reads its PROGRAM from stdin, so
					// any real remote consumes stdin before it can run at all. A fixture
					// that closed the channel without reading it modelled a server that
					// cannot exist, and raced the client's in-flight stdin write — the
					// copy failed with io.EOF, x/crypto's Wait() returned that in place
					// of the clean exit status, and the run was scored a failure. That
					// was a ~1-in-4 flake here for a long time; the same mechanism is
					// reachable in production (see the note in agent/ssh.go).
					_, _ = io.Copy(io.Discard, ch)
					_, _ = io.WriteString(ch, stdoutLine+"\n")
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					_ = ch.Close()
					return
				}
				_ = req.Reply(false, nil)
			}
		}()
	}
}

// e2eHarness holds the wired-up world for a single E2E run.
type e2eHarness struct {
	svc        *Service
	authSvc    *auth.Service
	server     *httptest.Server
	sshAddr    string
	agentKey   string // path to the agent's local private key (model b)
	knownHosts string // path to the agent's known_hosts (host-key verification)
}

// newE2EHarness builds the full real-server/real-agent fixture: a migrated DB,
// the real handlers mounted with the real RequireRunner middleware, and an
// in-test SSH server the agent will execute against.
//
// sensitiveStdout is the line the SSH target prints; seed it as an env_var value
// (below) so the ingest path's redactor masks it — proving server-side redaction.
func newE2EHarness(t *testing.T, sensitiveStdout string) *e2eHarness {
	t.Helper()

	// Agent's OWN key (model b): the server only references it by env-var NAME.
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}

	sshAddr, hostKey := e2eSSHServer(t, clientSigner.PublicKey(), sensitiveStdout)
	host, portStr, _ := net.SplitHostPort(sshAddr)
	port, _ := strconv.Atoi(portStr)

	// known_hosts so the agent verifies the in-test server strictly. A
	// non-standard port requires the "[host]:port" hostname form knownhosts
	// expects (matching what the dial presents).
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	khHost := host
	if port != 22 {
		khHost = "[" + host + "]:" + portStr
	}
	khLine := khHost + " " + string(ssh.MarshalAuthorizedKey(hostKey))
	if err := os.WriteFile(knownHosts, []byte(khLine), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t)
	as := authSvc(t, svc)

	// Mount the real runner routes the way runners_mount.go does — the minimal
	// subset the agent drives — with the real auth middleware.
	mux := http.NewServeMux()
	// register: the handler does its own registration-token auth (bootstrap token).
	mux.HandleFunc("POST /api/v1/runners/register", svc.HandleRegisterRunner)
	// poll / manifest / log: real RequireRunner (bearer = the per-runner apiKey).
	mux.Handle("GET /api/v1/runners/{id}/poll",
		as.RequireRunner(http.HandlerFunc(svc.HandlePoll)))
	mux.Handle("GET /api/v1/runs/{traceId}/manifest",
		as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest)))
	mux.Handle("POST /api/v1/runs/{traceId}/log",
		as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &e2eHarness{
		svc:        svc,
		authSvc:    as,
		server:     srv,
		sshAddr:    sshAddr,
		agentKey:   keyPath,
		knownHosts: knownHosts,
	}
}

// seedRunnerRun seeds a queued executor='runner' bash run plus the supporting
// job + scope→host→ssh_hosts rows the manifest resolution needs. The ssh_hosts
// row's address/port point at the in-test SSH server so the agent's manifest
// carries a target it can actually dial. Returns the seeded run's trace id.
func (h *e2eHarness) seedRunnerRun(t *testing.T, jobName, scope, hostName, authKeyEnvVar string) string {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(h.sshAddr)
	port, _ := strconv.Atoi(portStr)
	ts := now()

	// Job command body the manifest resolves via execspec.ResolveCommand.
	insertJobDef(t, h.svc, jobName, "bash", "echo from-job", 0)

	// scope → host wiring, with an ssh_hosts row pointed at the in-test server.
	scopeID := db.NewID()
	if _, err := h.svc.db.Exec(
		`INSERT INTO scopes(id, name, source, created_at) VALUES (?, ?, 'cronomicon', ?)`,
		scopeID, scope, ts); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	if _, err := h.svc.db.Exec(
		`INSERT INTO scope_hosts(scope_id, host) VALUES (?, ?)`, scopeID, hostName); err != nil {
		t.Fatalf("seed scope_host: %v", err)
	}
	if _, err := h.svc.db.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, created_at)
		VALUES (?, ?, ?, ?, 'tester', ?, ?)`,
		db.NewID(), hostName, host, port, authKeyEnvVar, ts); err != nil {
		t.Fatalf("seed ssh_host: %v", err)
	}

	// The queued runner run the poll loop will claim.
	traceID := db.NewTraceID()
	if _, err := h.svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES (?, ?, 'bash', ?, 'queued', 'test', 'manual', 'runner', ?)`,
		traceID, jobName, scope, ts); err != nil {
		t.Fatalf("seed queued run: %v", err)
	}
	return traceID
}

// agentConfig returns an agent.Config wired to the harness (httptest server,
// in-test SSH target key, fast poll cadence). authKeyEnvVar maps to the agent's
// local key file (model b) so the SSH hop authenticates.
func (h *e2eHarness) agentConfig(t *testing.T, name, authKeyEnvVar string) agent.Config {
	t.Helper()
	return agent.Config{
		ServerURL:         h.server.URL,
		RegistrationToken: "test-bootstrap-token", // matches newTestService's cfg
		Name:              name,
		OS:                "Linux",
		Capabilities:      []string{"bash"},
		MaxConcurrent:     2,
		Inventory:         "cronomicon",
		IdentityFile:      filepath.Join(t.TempDir(), "id.json"),
		PollInterval:      20 * time.Millisecond,
		KeyMap:            map[string]string{authKeyEnvVar: h.agentKey},
		KnownHostsFile:    h.knownHosts,
		LogRetryBudget:    5,
		FanOut:            4,
	}
}

// waitFor polls cond up to the timeout, returning true once it holds. Used to
// assert DB state transitions deterministically rather than racing fixed sleeps.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// runStatus reads a run's status (and runner_id) from the DB.
func (h *e2eHarness) runStatus(t *testing.T, traceID string) (status, runnerID string) {
	t.Helper()
	var rid string
	_ = h.svc.db.QueryRow(
		`SELECT status, COALESCE(runner_id,'') FROM runs WHERE id = ?`, traceID).
		Scan(&status, &rid)
	return status, rid
}

// ── Happy path ────────────────────────────────────────────────────────────────

// TestRunnerAgentE2EHappyPath drives the real agent against the real server
// handlers and asserts the full lifecycle: register → claim → manifest →
// SSH-execute → log ingest (redacted) → terminal status.
func TestRunnerAgentE2EHappyPath(t *testing.T) {
	const secret = "TOP-SECRET-OUTPUT"
	h := newE2EHarness(t, secret)

	// Seed a Variable whose value equals the SSH target's stdout so we can prove
	// single-line Variables are LOG-VISIBLE (D7): the ingest redactor no longer
	// masks them. env_vars scope '*' applies to any run.
	if _, err := h.svc.db.Exec(`
		INSERT INTO env_vars(id, key, scope, value, created_at)
		VALUES (?, 'SECRET', '*', ?, ?)`, db.NewID(), secret, now()); err != nil {
		t.Fatalf("seed env_var: %v", err)
	}

	traceID := h.seedRunnerRun(t, "deploy-job", "prod", "web01", "AGENT_KEY")

	cfg := h.agentConfig(t, "e2e-runner", "AGENT_KEY")
	a, err := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	// 1. Registration persists a runners row + the apiKey is usable (the poll/
	//    manifest/log calls below all go through RequireRunner with that key).
	if !waitFor(t, 5*time.Second, func() bool {
		var n int
		_ = h.svc.db.QueryRow(`SELECT COUNT(1) FROM runners WHERE name = 'e2e-runner'`).Scan(&n)
		return n == 1
	}) {
		cancel()
		<-done
		t.Fatal("agent did not register a runner row")
	}
	var runnerID string
	_ = h.svc.db.QueryRow(`SELECT id FROM runners WHERE name = 'e2e-runner'`).Scan(&runnerID)
	// A bound runner_tokens row must exist (RequireRunner authorizes by it, R1.4).
	var tokenCount int
	_ = h.svc.db.QueryRow(`SELECT COUNT(1) FROM runner_tokens WHERE runner_id = ?`, runnerID).Scan(&tokenCount)
	if tokenCount != 1 {
		t.Errorf("expected 1 bound runner_token, got %d", tokenCount)
	}

	// 2 + 3. Poll claims the run (queued→running, runner_id set), then the agent
	//        fetches the manifest and executes; the run finalizes via the
	//        trailing envelope. Wait for the terminal state.
	if !waitFor(t, 6*time.Second, func() bool {
		st, _ := h.runStatus(t, traceID)
		return st == "success"
	}) {
		st, rid := h.runStatus(t, traceID)
		cancel()
		<-done
		t.Fatalf("run did not reach success: status=%q runner_id=%q", st, rid)
	}

	st, rid := h.runStatus(t, traceID)
	if st != "success" {
		t.Errorf("terminal status = %q, want success", st)
	}
	if rid != runnerID {
		t.Errorf("run runner_id = %q, want claiming runner %q", rid, runnerID)
	}

	// Exit code + duration came from the agent's trailing envelope.
	var exitCode int
	var durationMs int64
	if err := h.svc.db.QueryRow(
		`SELECT exit_code, COALESCE(duration_ms,0) FROM runs WHERE id = ?`, traceID).
		Scan(&exitCode, &durationMs); err != nil {
		t.Fatalf("read run terminals: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit_code = %d, want 0", exitCode)
	}

	// 4. The per-run log file was written by the server, carries the host-prefixed
	//    SSH output, and the Variable value is log-visible (D7 — single-line
	//    Variables are no longer redacted; injected-secret masking is covered by
	//    TestIngestMasksInjectedSecret / TestIngestMasksDeliveredKeyMaterial).
	lines, err := linesTo(mustLogPath(t, h.svc, traceID))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	logStr := strings.Join(lines, "\n")
	if !strings.Contains(logStr, "[web01]") {
		t.Errorf("log missing host-prefixed SSH output:\n%s", logStr)
	}
	if !strings.Contains(logStr, secret) {
		t.Errorf("Variable value should be log-visible (D7), but is missing/masked:\n%s", logStr)
	}
	if strings.Contains(logStr, redactMask) {
		t.Errorf("unexpected [REDACTED] marker for a plain Variable:\n%s", logStr)
	}

	// A run-end activity row was emitted (the notify/metrics seam fires off this).
	// It is written asynchronously, so poll rather than read once — a bare read
	// races the seam under CPU contention (flaky "got 0").
	activityCount := 0
	if !waitFor(t, 2*time.Second, func() bool {
		_ = h.svc.db.QueryRow(
			`SELECT COUNT(1) FROM activity WHERE kind = 'run-end' AND trace_id = ?`, traceID).
			Scan(&activityCount)
		return activityCount == 1
	}) {
		t.Errorf("expected 1 run-end activity, got %d", activityCount)
	}

	cancel()
	<-done
}

// ── Kill sub-case ──────────────────────────────────────────────────────────────

// blockingSSHServer accepts the client key and, on exec, holds the session open
// (the command "runs" indefinitely) until the agent tears the connection down —
// which is exactly what the agent's kill path does: it cancels the run ctx,
// which signals + closes the SSH session and then the client. started closes
// once the exec begins (the run is provably executing); torndown closes once the
// SSH connection is closed by the agent (the kill took effect).
func blockingSSHServer(t *testing.T, clientPub ssh.PublicKey, started, torndown chan<- struct{}) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var startOnce, downOnce sync.Once
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(nConn net.Conn) {
				sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
				if err != nil {
					return
				}
				defer sshConn.Close()
				go ssh.DiscardRequests(reqs)

				// connClosed fires when the underlying SSH connection is torn down —
				// which is exactly what the agent's kill path does (it cancels the
				// run ctx → closes the SSH session/client). We block the exec on this
				// rather than on channel stdin (which the agent closes immediately,
				// since the run has no stdin), so the command stays "running" until
				// the kill actually lands.
				connClosed := make(chan struct{})
				go func() {
					_ = sshConn.Wait()
					close(connClosed)
					downOnce.Do(func() { close(torndown) })
				}()

				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "only session")
						continue
					}
					ch, chReqs, err := newCh.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range chReqs {
							if req.Type == "exec" {
								_ = req.Reply(true, nil)
								_, _ = io.WriteString(ch, "starting long task\n")
								startOnce.Do(func() { close(started) })
								// Block until the agent tears down the connection on
								// kill, or a generous safety deadline elapses.
								select {
								case <-connClosed:
								case <-time.After(20 * time.Second):
								}
								_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{137}))
								_ = ch.Close()
								return
							}
							_ = req.Reply(false, nil)
						}
					}()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

// TestRunnerAgentE2EKill is the deterministic kill E2E. It uses a blocking SSH
// target so the run is provably executing when the kill is queued: an
// action_queue 'kill' row is inserted for the trace, the next poll delivers the
// control op (B5→B4 seam), the agent cancels the run's execution context (which
// tears down the SSH session), and the run ends non-success.
func TestRunnerAgentE2EKill(t *testing.T) {
	// Agent's local key.
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	torndown := make(chan struct{})
	sshAddr, hostKey := blockingSSHServer(t, clientSigner.PublicKey(), started, torndown)
	host, portStr, _ := net.SplitHostPort(sshAddr)
	port, _ := strconv.Atoi(portStr)

	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	khHost := "[" + host + "]:" + portStr
	khLine := khHost + " " + string(ssh.MarshalAuthorizedKey(hostKey))
	if err := os.WriteFile(knownHosts, []byte(khLine), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t)
	as := authSvc(t, svc)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/runners/register", svc.HandleRegisterRunner)
	mux.Handle("GET /api/v1/runners/{id}/poll", as.RequireRunner(http.HandlerFunc(svc.HandlePoll)))
	mux.Handle("GET /api/v1/runs/{traceId}/manifest", as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest)))
	mux.Handle("POST /api/v1/runs/{traceId}/log", as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Seed job + host (pointed at the blocking SSH server) + queued run.
	ts := now()
	insertJobDef(t, svc, "long-job", "bash", "sleep 100", 0)
	scopeID := db.NewID()
	if _, err := svc.db.Exec(`INSERT INTO scopes(id, name, source, created_at) VALUES (?, 'prod', 'cronomicon', ?)`, scopeID, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO scope_hosts(scope_id, host) VALUES (?, 'web01')`, scopeID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, created_at)
		VALUES (?, 'web01', ?, ?, 'tester', 'AGENT_KEY', ?)`, db.NewID(), host, port, ts); err != nil {
		t.Fatal(err)
	}
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES (?, 'long-job', 'bash', 'prod', 'queued', 'test', 'manual', 'runner', ?)`, traceID, ts); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		ServerURL:         srv.URL,
		RegistrationToken: "test-bootstrap-token",
		Name:              "kill-runner",
		OS:                "Linux",
		Capabilities:      []string{"bash"},
		MaxConcurrent:     2,
		Inventory:         "cronomicon",
		IdentityFile:      filepath.Join(t.TempDir(), "id.json"),
		PollInterval:      20 * time.Millisecond,
		KeyMap:            map[string]string{"AGENT_KEY": keyPath},
		KnownHostsFile:    knownHosts,
		LogRetryBudget:    5,
		FanOut:            4,
	}
	a, err := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	// Wait until the SSH target reports the exec started — the run is provably
	// executing now (claimed + manifest fetched + SSH session open).
	select {
	case <-started:
	case <-time.After(8 * time.Second):
		cancel()
		<-done
		t.Fatal("SSH exec never started — run did not reach execution")
	}

	// Run must be 'running' and owned by the kill-runner.
	var runnerID string
	_ = svc.db.QueryRow(`SELECT id FROM runners WHERE name = 'kill-runner'`).Scan(&runnerID)
	if !waitFor(t, 3*time.Second, func() bool {
		var st, rid string
		_ = svc.db.QueryRow(`SELECT status, COALESCE(runner_id,'') FROM runs WHERE id=?`, traceID).Scan(&st, &rid)
		return st == "running" && rid == runnerID
	}) {
		cancel()
		<-done
		t.Fatal("run not in running state before kill")
	}

	// Operator kill: B5 enqueues an action_queue 'kill' row for this run. The
	// next poll delivers it (drainControl, B5→B4 seam) and the agent cancels the
	// run's execution context.
	if _, err := svc.db.Exec(`
		INSERT INTO action_queue(run_id, op, created_at) VALUES (?, 'kill', ?)`,
		traceID, now()); err != nil {
		t.Fatal(err)
	}

	// Assert the kill TOOK EFFECT: the agent tore down the SSH connection. This is
	// the observable end-to-end proof that the control op reached the agent and it
	// cancelled the run's execution context (which signals + closes the session).
	//
	// Note on terminal status: on kill the agent's final log stream runs under the
	// run's now-cancelled context, so it does NOT push a trailing envelope — the
	// killed run is left 'running' for the server-side reaper (R3) to reconcile.
	// We therefore assert the kill's direct effects (session torn down + control
	// consumed) rather than a DB terminal status, which a separate reaper test
	// owns.
	select {
	case <-torndown:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("kill did not tear down the SSH session within 10s — control op not applied")
	}

	// The run must NOT have succeeded (a kill is a non-success outcome).
	var st string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, traceID).Scan(&st)
	if st == "success" {
		t.Errorf("killed run should not be success, got %q", st)
	}

	// The kill control op was consumed (B5→B4 seam): consumed_at set.
	if !waitFor(t, 3*time.Second, func() bool {
		var consumed int
		_ = svc.db.QueryRow(`SELECT COUNT(1) FROM action_queue WHERE run_id=? AND consumed_at IS NOT NULL`, traceID).Scan(&consumed)
		return consumed == 1
	}) {
		t.Error("expected the kill action_queue row to be consumed (consumed_at set)")
	}

	cancel()
	<-done
}

// ── Drain sub-case ─────────────────────────────────────────────────────────────

// TestRunnerAgentE2EDrain exercises drain end-to-end: an already-registered,
// idle agent is drained (runners.status → draining); its next poll returns the
// drain control op, the agent stops claiming new work, and a freshly-queued run
// stays queued (the drained agent never claims it).
func TestRunnerAgentE2EDrain(t *testing.T) {
	h := newE2EHarness(t, "drain-output")

	cfg := h.agentConfig(t, "drain-runner", "AGENT_KEY")
	a, err := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	// Wait for registration.
	var runnerID string
	if !waitFor(t, 5*time.Second, func() bool {
		_ = h.svc.db.QueryRow(`SELECT COALESCE(id,'') FROM runners WHERE name='drain-runner'`).Scan(&runnerID)
		return runnerID != ""
	}) {
		cancel()
		<-done
		t.Fatal("drain-runner did not register")
	}

	// Operator drain: flip the runner to draining (the poll handler returns a
	// 'drain' control op for draining runners and never hands out work).
	if _, err := h.svc.db.Exec(`UPDATE runners SET status='draining' WHERE id=?`, runnerID); err != nil {
		t.Fatal(err)
	}

	// Let the drain propagate. Two things must settle: (a) any server long-poll
	// already in-flight when we flipped the status must re-check and return drain
	// control (the server re-checks every pollInterval = 500ms), and (b) the agent
	// must observe that drain control and set its internal draining flag. Wait
	// comfortably longer than the server's 500ms re-check so neither path can
	// still be holding a pre-drain claim window.
	time.Sleep(1500 * time.Millisecond)

	traceID := h.seedRunnerRun(t, "post-drain-job", "prod", "web01", "AGENT_KEY")

	// The drained agent must NOT claim the new run: it stays queued. Assert it
	// stays queued across a window comfortably longer than several poll cycles.
	stayedQueued := true
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		_ = h.svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, traceID).Scan(&st)
		if st != "queued" {
			stayedQueued = false
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stayedQueued {
		var st string
		_ = h.svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, traceID).Scan(&st)
		t.Errorf("drained agent claimed a run queued after drain (status=%q); drain did not stop new work", st)
	}

	cancel()
	<-done
}

// ── Notes on log-resume ────────────────────────────────────────────────────────
//
// R8.2 also lists "resume after a dropped connection". The server side of resume
// (X-Resume-Offset bookkeeping, 409 on mismatch, append-from-offset) is covered
// deterministically by TestResumeOffset in this package, and the agent side
// (resume budget + offset tracking across a dropped POST) by
// internal/agent/logstream_test.go. An end-to-end variant would have to force a
// mid-stream connection drop and race the agent's internal retry loop, which is
// inherently timing-sensitive; per the task's robustness guidance we rely on the
// two deterministic unit tests above rather than ship a flaky E2E for resume.
