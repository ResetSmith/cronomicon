package runner

// Stopping an agent (2.3.2), end to end: the REAL agent against the REAL
// handlers, as in e2e_test.go. Until 2.3.2 a stop signal killed every run in
// flight and cancelled the upload of its log with it, so the server never
// learned the outcome and — the agent being back within the offline window —
// nothing ever closed the row. Three things are held here:
//
//   - the signal is a drain: the run in flight finishes and is recorded, and
//     the agent takes no new run while it does;
//   - a second signal (Agent.Abort) cancels the run, and the run still reports;
//   - a run that an agent process left behind is closed by the first poll of
//     the process that replaces it.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"golang.org/x/crypto/ssh"
)

// heldSSHServer accepts the client key and, on exec, holds the command open
// until release is closed (the command then exits 0) or the agent tears the
// connection down (exit 137, and torndown closes). started closes when the
// first exec begins.
func heldSSHServer(t *testing.T, clientPub ssh.PublicKey, started chan<- struct{}, release <-chan struct{}, torndown chan<- struct{}) (addr string, hostKey ssh.PublicKey) {
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
	t.Cleanup(func() { _ = ln.Close() })

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
				defer func() { _ = sshConn.Close() }()
				go ssh.DiscardRequests(reqs)
				released := make(chan struct{})
				connClosed := make(chan struct{})
				go func() {
					_ = sshConn.Wait()
					close(connClosed)
					select {
					case <-released: // the command ended by itself; the close is the client leaving
					default:
						downOnce.Do(func() { close(torndown) })
					}
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
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							_ = req.Reply(true, nil)
							_, _ = io.WriteString(ch, "starting long task\n")
							startOnce.Do(func() { close(started) })
							code := uint32(137)
							select {
							case <-release:
								close(released)
								_, _ = io.WriteString(ch, "long task done\n")
								code = 0
							case <-connClosed:
							case <-time.After(30 * time.Second):
							}
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
							_ = ch.Close()
							return
						}
					}()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

// stopHarness is one server, one held SSH target and one queued run on it.
type stopHarness struct {
	svc      *Service
	srv      *httptest.Server
	cfg      agent.Config
	traceID  string
	started  chan struct{}
	release  chan struct{}
	torndown chan struct{}
}

func newStopHarness(t *testing.T, runnerName string) *stopHarness {
	t.Helper()
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

	h := &stopHarness{started: make(chan struct{}), release: make(chan struct{}), torndown: make(chan struct{})}
	sshAddr, hostKey := heldSSHServer(t, clientSigner.PublicKey(), h.started, h.release, h.torndown)
	host, portStr, _ := net.SplitHostPort(sshAddr)
	port, _ := strconv.Atoi(portStr)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	khLine := "[" + host + "]:" + portStr + " " + string(ssh.MarshalAuthorizedKey(hostKey))
	if err := os.WriteFile(knownHosts, []byte(khLine), 0o600); err != nil {
		t.Fatal(err)
	}

	h.svc = newTestService(t)
	as := authSvc(t, h.svc)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/runners/register", h.svc.HandleRegisterRunner)
	mux.Handle("GET /api/v1/runners/{id}/poll", as.RequireRunner(http.HandlerFunc(h.svc.HandlePoll)))
	mux.Handle("GET /api/v1/runs/{traceId}/manifest", as.RequireRunner(http.HandlerFunc(h.svc.HandleGetManifest)))
	mux.Handle("POST /api/v1/runs/{traceId}/log", as.RequireRunner(http.HandlerFunc(h.svc.HandleIngestLog)))
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)

	ts := now()
	insertJobDef(t, h.svc, "long-job", "bash", "sleep 100", 0)
	scopeID := db.NewID()
	if _, err := h.svc.db.Exec(`INSERT INTO scopes(id, name, source, created_at) VALUES (?, 'prod', 'cronomicon', ?)`, scopeID, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.db.Exec(`INSERT INTO scope_hosts(scope_id, host) VALUES (?, 'web01')`, scopeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.db.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var, created_at)
		VALUES (?, 'web01', ?, ?, 'tester', 'AGENT_KEY', ?)`, db.NewID(), host, port, ts); err != nil {
		t.Fatal(err)
	}
	h.traceID = h.queueRun(t)

	h.cfg = agent.Config{
		ServerURL:         h.srv.URL,
		RegistrationToken: "test-bootstrap-token",
		Name:              runnerName,
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
	return h
}

// queueRun queues one more run of the long job.
func (h *stopHarness) queueRun(t *testing.T) string {
	t.Helper()
	traceID := db.NewTraceID()
	if _, err := h.svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES (?, 'long-job', 'bash', 'prod', 'queued', 'test', 'manual', 'runner', ?)`, traceID, now()); err != nil {
		t.Fatal(err)
	}
	return traceID
}

func (h *stopHarness) run(traceID string) (status, reason string) {
	_ = h.svc.db.QueryRow(`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE id = ?`, traceID).Scan(&status, &reason)
	return status, reason
}

// start runs an agent and waits for the queued run to be executing on it.
func (h *stopHarness) start(t *testing.T) (a *agent.Agent, stop context.CancelFunc, done chan struct{}) {
	t.Helper()
	a, err := agent.New(h.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	t.Cleanup(func() {
		// Whatever the test left running: end it, so Run returns.
		cancel()
		a.Abort()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	select {
	case <-h.started:
	case <-time.After(8 * time.Second):
		t.Fatal("the run never started executing")
	}
	if !waitFor(t, 3*time.Second, func() bool { st, _ := h.run(h.traceID); return st == "running" }) {
		t.Fatal("the run is not running")
	}
	return a, cancel, done
}

// TestRunnerAgentE2EStopSignalDrains: the stop signal ends the claiming of
// work, not the work.
func TestRunnerAgentE2EStopSignalDrains(t *testing.T) {
	h := newStopHarness(t, "stop-runner")
	_, stop, done := h.start(t)

	stop() // SIGTERM, as cmd/cronomicon-runner delivers it

	// A run queued once the agent is stopping must be left alone...
	later := h.queueRun(t)
	// ...and the run in flight must be neither cut off nor abandoned.
	select {
	case <-h.torndown:
		t.Fatal("the stop signal tore down the run's SSH session: the run was killed, not drained")
	case <-done:
		t.Fatal("the agent exited with a run still executing")
	case <-time.After(1500 * time.Millisecond):
	}
	if st, _ := h.run(h.traceID); st != "running" {
		t.Fatalf("run in flight: status %q while the agent drains, want running", st)
	}
	if st, _ := h.run(later); st != "queued" {
		t.Fatalf("a stopping agent claimed a run (status %q)", st)
	}
	// The heartbeat goes on while it drains, or the reaper would take the runner
	// (and this run with it) at RunnerOfflineAfter.
	var before, after string
	_ = h.svc.db.QueryRow(`SELECT last_seen_at FROM runners WHERE name = 'stop-runner'`).Scan(&before)
	time.Sleep(1200 * time.Millisecond)
	_ = h.svc.db.QueryRow(`SELECT last_seen_at FROM runners WHERE name = 'stop-runner'`).Scan(&after)
	if after == before {
		t.Errorf("no heartbeat while draining: last_seen_at stayed %s", before)
	}

	close(h.release) // the job finishes by itself

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit after its last run finished")
	}
	if st, _ := h.run(h.traceID); st != "success" {
		t.Errorf("drained run: status %q, want success", st)
	}
	if st, _ := h.run(later); st != "queued" {
		t.Errorf("the run queued during the drain: status %q, want queued", st)
	}
}

// TestRunnerAgentE2EAbortCancelsAndReports: a second signal cancels the runs,
// and each still reports what happened to it.
func TestRunnerAgentE2EAbortCancelsAndReports(t *testing.T) {
	h := newStopHarness(t, "abort-runner")
	a, stop, done := h.start(t)

	stop()
	time.Sleep(200 * time.Millisecond)
	a.Abort() // the second signal

	select {
	case <-h.torndown:
	case <-time.After(10 * time.Second):
		t.Fatal("Abort did not end the run")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit after Abort")
	}
	st, reason := h.run(h.traceID)
	if st != "failure" {
		t.Fatalf("aborted run: status %q (%s), want failure: the agent must report a run it cancelled", st, reason)
	}
	logPath := mustLogPath(t, h.svc, h.traceID)
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read the run's log (%q): %v", logPath, err)
	}
	if !strings.Contains(string(body), "the runner agent was stopped before this run finished") {
		t.Errorf("the log does not say why the run ended:\n%s", body)
	}
}

// TestRunnerAgentE2ERestartClosesLostRuns: an agent process that dies with a
// run leaves it `running`; the process that replaces it says it has started,
// and the server closes the run. Without that the row stays running for good,
// because the runner is never quiet long enough for the reaper.
func TestRunnerAgentE2ERestartClosesLostRuns(t *testing.T) {
	h := newStopHarness(t, "restart-runner")
	h.start(t)

	var runnerID, token string
	_ = h.svc.db.QueryRow(`SELECT id FROM runners WHERE name = 'restart-runner'`).Scan(&runnerID)
	idFile, err := os.ReadFile(h.cfg.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	var ident struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(idFile, &ident); err != nil || ident.APIKey == "" {
		t.Fatalf("read the agent's identity: %v (%s)", err, idFile)
	}
	token = ident.APIKey

	// An ordinary poll of the same runner (its process is alive and holds the
	// run) closes nothing.
	if code, _ := pollWith(t, h.svc, runnerID, token, runnerproto.PollParamClaim+"=0"); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("poll: %d", code)
	}
	if st, _ := h.run(h.traceID); st != "running" {
		t.Fatalf("a poll that did not announce a start closed the run (status %q)", st)
	}

	// The first poll of a replacement process.
	if code, _ := pollWith(t, h.svc, runnerID, token, runnerproto.PollParamStarted+"=1&"+runnerproto.PollParamClaim+"=0"); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("poll: %d", code)
	}
	st, reason := h.run(h.traceID)
	if st != "failure" || reason != "runner_lost" {
		t.Errorf("the lost run: status %q reason %q, want failure / runner_lost", st, reason)
	}
	var load int
	_ = h.svc.db.QueryRow(`SELECT load FROM runners WHERE id = ?`, runnerID).Scan(&load)
	if load != 0 {
		t.Errorf("runner load after the restart: %d, want 0", load)
	}
	var ends int
	_ = h.svc.db.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind = 'run-end' AND trace_id = ?`, h.traceID).Scan(&ends)
	if ends != 1 {
		t.Errorf("run-end activity rows for the lost run: %d, want 1", ends)
	}
}

// TestRunnerAgentE2EFullAgentIsNotHandedWork: an agent with every slot taken
// asks for nothing. The claim does not know how many runs an agent holds, so
// until 2.3.2 a full agent that polled was handed the next queued run, could
// not start it, and the run stayed `running` on that runner for good: never
// executed, never reported, and counted in the runner's load.
func TestRunnerAgentE2EFullAgentIsNotHandedWork(t *testing.T) {
	h := newStopHarness(t, "full-runner")
	h.cfg.MaxConcurrent = 1
	h.start(t) // its one slot is taken by a run that is held open

	second := h.queueRun(t)
	// Many poll cycles (the cadence here is 20ms): the second run must wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := h.run(second); st != "queued" {
			t.Fatalf("a full agent was handed a second run (status %q): it cannot start it, and nothing would end it", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var load int
	_ = h.svc.db.QueryRow(`SELECT load FROM runners WHERE name = 'full-runner'`).Scan(&load)
	if load != 1 {
		t.Errorf("runner load = %d with one run in flight and a limit of one", load)
	}

	close(h.release) // the first run finishes; the slot opens

	if !waitFor(t, 10*time.Second, func() bool { st, _ := h.run(second); return st == "success" }) {
		st, reason := h.run(second)
		t.Fatalf("the waiting run was not taken once a slot opened: status %q (%s)", st, reason)
	}
	if st, _ := h.run(h.traceID); st != "success" {
		t.Errorf("the first run: status %q, want success", st)
	}
}

// TestRunnerAgentE2EQueuedRunsStartTogether: an agent with free slots takes the
// runs that are waiting one after another, at once. Until 2.3.2 it took one per
// poll interval (a minute by default): the server answered each assignment
// "come back now" and the agent waited for its next tick anyway.
func TestRunnerAgentE2EQueuedRunsStartTogether(t *testing.T) {
	h := newStopHarness(t, "burst-runner")
	h.cfg.MaxConcurrent = 5
	h.cfg.PollInterval = time.Minute // nothing below can be the next tick
	second, third := h.queueRun(t), h.queueRun(t)
	h.start(t)

	running := func(id string) bool { st, _ := h.run(id); return st == "running" }
	if !waitFor(t, 8*time.Second, func() bool { return running(h.traceID) && running(second) && running(third) }) {
		a, _ := h.run(h.traceID)
		b, _ := h.run(second)
		c, _ := h.run(third)
		t.Fatalf("three runs queued for an agent with five slots, 8s later: %s, %s, %s; want all running", a, b, c)
	}
	close(h.release)
	if !waitFor(t, 10*time.Second, func() bool {
		for _, id := range []string{h.traceID, second, third} {
			if st, _ := h.run(id); st != "success" {
				return false
			}
		}
		return true
	}) {
		t.Fatal("the three runs did not all succeed")
	}
}

// TestRunnerAgentE2EStopReachesAFullAgentQuickly: a kill is delivered as the
// answer to a poll. A full agent's polls claim nothing and are not held by the
// server, so without a heartbeat of its own a full agent would hear of a Stop
// only at its next tick, a minute by default, while the operator's write had
// already ended the run on the server and the job went on running on its
// target. An agent with a run in flight asks again within seconds.
func TestRunnerAgentE2EStopReachesAFullAgentQuickly(t *testing.T) {
	h := newStopHarness(t, "busy-runner")
	h.cfg.MaxConcurrent = 1          // full once the held run starts
	h.cfg.PollInterval = time.Minute // nothing below can be the next tick
	h.start(t)

	// Let the poll that followed the assignment (answered at once: the agent is
	// full) pass, so the kill below can only be delivered by a LATER poll.
	time.Sleep(500 * time.Millisecond)
	if _, err := h.svc.db.Exec(`INSERT INTO action_queue(run_id, op, created_at) VALUES (?, 'kill', ?)`, h.traceID, now()); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	select {
	case <-h.torndown:
	case <-time.After(15 * time.Second):
		t.Fatal("15s after a Stop the run was still executing on a full agent: it waited for its next tick")
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("the Stop took %s to reach a full agent", took.Round(time.Millisecond))
	}
}

// pollWith is pollAs with a query string.
func pollWith(t *testing.T, svc *Service, runnerID, token, query string) (int, runnerproto.PollResponse) {
	t.Helper()
	as := authSvc(t, svc)
	h := as.RequireRunner(http.HandlerFunc(svc.HandlePoll))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runners/"+runnerID+"/poll?"+query, nil)
	req.SetPathValue("id", runnerID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var pr runnerproto.PollResponse
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &pr)
	}
	return rec.Code, pr
}

// TestPollClaimZeroClaimsNothing: a stopping agent's poll is a heartbeat that
// is never answered with work, and is not held.
func TestPollClaimZeroClaimsNothing(t *testing.T) {
	svc := newTestService(t)
	id, tok := "runner-stopping", "crn_run_stopping"
	insertRunner(t, svc, id, "stopping", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)
	insertJobDef(t, svc, "waiting-job", "bash", "echo hi", 0)
	traceID := db.NewTraceID()
	insertQueuedRun(t, svc, traceID, "waiting-job", "bash", "")

	began := time.Now()
	code, pr := pollWith(t, svc, id, tok, runnerproto.PollParamClaim+"=0")
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("the poll was held for %s; a stopping agent's poll is answered at once", took)
	}
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("poll: %d", code)
	}
	if pr.Assignment != nil {
		t.Fatalf("a stopping agent was handed run %s", pr.Assignment.TraceID)
	}
	var st string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, traceID).Scan(&st)
	if st != "queued" {
		t.Errorf("the waiting run: status %q, want queued", st)
	}
	var seen string
	_ = svc.db.QueryRow(`SELECT COALESCE(last_seen_at,'') FROM runners WHERE id = ?`, id).Scan(&seen)
	if seen == "" {
		t.Error("the poll was not counted as a heartbeat")
	}

	// The same runner, not stopping, does take it: the queued run was claimable.
	_, pr = pollAs(t, svc, id, tok)
	if pr.Assignment == nil || pr.Assignment.TraceID != traceID {
		t.Errorf("an ordinary poll did not claim the run: %+v", pr.Assignment)
	}
}
