package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// stopServer is a server for the edges of a stop: it holds a manifest until it
// is released, takes its time over a log upload, and counts a known_hosts
// report only when the agent stayed on the line for the answer.
type stopServer struct {
	mu      sync.Mutex
	polls   int
	answer  func(n int, query string) string
	logs    map[string]string // trace id → body of each COMPLETED upload
	reports int               // known_hosts reports taken whole

	manifestAsked chan struct{} // receives when a manifest is asked for
	release       chan struct{} // closed to let the manifest answer (404)
	releaseOnce   sync.Once
	logArrived    chan struct{} // receives when a log upload begins
	logDelay      time.Duration // how long a log upload takes to be answered

	srv *httptest.Server
}

func newStopServer(t *testing.T, answer func(n int, query string) string) *stopServer {
	t.Helper()
	s := &stopServer{
		answer:        answer,
		logs:          map[string]string{},
		manifestAsked: make(chan struct{}, 8),
		release:       make(chan struct{}),
		logArrived:    make(chan struct{}, 8),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/runners/register"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"runner":{"id":"r1"},"apiKey":"crn_run_x"}`))
		case strings.HasSuffix(r.URL.Path, "/poll"):
			s.mu.Lock()
			s.polls++
			body := ""
			if s.answer != nil {
				body = s.answer(s.polls, r.URL.RawQuery)
			}
			s.mu.Unlock()
			if body == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/known-hosts"):
			_, _ = io.Copy(io.Discard, r.Body)
			// An upload whose context was cancelled hangs up here.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			s.mu.Lock()
			s.reports++
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/manifest"):
			select {
			case s.manifestAsked <- struct{}{}:
			default:
			}
			select {
			case <-s.release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/log"):
			select {
			case s.logArrived <- struct{}{}:
			default:
			}
			body, _ := io.ReadAll(r.Body)
			time.Sleep(s.logDelay)
			trace := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/runs/"), "/log")
			s.mu.Lock()
			s.logs[trace] += string(body)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		s.releaseManifest()
		s.srv.Close()
	})
	return s
}

func (s *stopServer) releaseManifest() { s.releaseOnce.Do(func() { close(s.release) }) }

func (s *stopServer) reportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports
}

func (s *stopServer) log(trace string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logs[trace]
}

// start runs a real agent against the server, with an hour's poll interval of
// its own. It returns the stop signal and a channel closed when Run returns.
func (s *stopServer) start(t *testing.T) (stop context.CancelFunc, done chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	a, err := New(Config{
		ServerURL: s.srv.URL, RegistrationToken: "crn_reg_once", Name: "r", OS: "Linux",
		Capabilities: []string{"bash"}, MaxConcurrent: 5, Inventory: "cronomicon",
		IdentityFile:   filepath.Join(dir, "identity.json"),
		KnownHostsFile: filepath.Join(dir, "known_hosts"),
		PollInterval:   time.Hour, NoSandbox: true, LogRetryBudget: 1,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	t.Cleanup(func() {
		stop()
		s.releaseManifest()
		a.Abort()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not exit")
		}
	})
	return stop, done
}

const stopEdgeAssignment = `{"assignment":{"traceId":"t1","jobName":"j","type":"bash","scope":"","payload":{}},"control":[],"pollAfterMs":0}`

// A host-key op that reaches an agent while it is finishing its runs is still
// carried out. The server hands such an op over once and marks it delivered, so
// an agent that takes it and drops it leaves an operator waiting on a review
// screen for a scan or a report that will never arrive. The drain's heartbeat
// poll has a context of its own, ended as soon as the poll is answered; the
// work the poll starts must not hang from it.
func TestAHostKeyReportAskedForDuringADrainIsStillSent(t *testing.T) {
	asked := false
	s := newStopServer(t, func(n int, query string) string {
		if n == 1 {
			return stopEdgeAssignment
		}
		if strings.Contains(query, runnerproto.PollParamClaim+"=0") && !asked {
			asked = true
			return `{"assignment":null,"control":[{"op":"known-hosts-report"}],"pollAfterMs":0}`
		}
		return ""
	})
	stop, done := s.start(t)

	select {
	case <-s.manifestAsked: // the run is in flight, and stays so until released
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never started the run")
	}
	// The report every agent sends once at startup, so the one below is the
	// one the server asked for.
	if !eventually(5*time.Second, func() bool { return s.reportCount() >= 1 }) {
		t.Fatal("no known_hosts report at startup")
	}
	before := s.reportCount()

	stop() // SIGTERM with a run in flight: the agent drains
	if !eventually(5*time.Second, func() bool { return s.reportCount() > before }) {
		t.Fatal("a known_hosts report the server asked for during the drain was never uploaded: " +
			"the op was taken from the server and dropped")
	}

	s.releaseManifest()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit once its run had ended")
	}
}

// A run that is cancelled before its manifest arrives is still ENDED on the
// server, like a run cancelled at any later point: one line and the envelope of
// a failure. The upload used the run's own, cancelled context, so nothing was
// sent and the run stayed `running` until the agent's next start.
func TestARunStoppedBeforeItsManifestArrivesIsStillReported(t *testing.T) {
	s := newStopServer(t, nil)
	c, err := NewClient(s.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		cfg:    Config{ServerURL: s.srv.URL, Name: "r", MaxConcurrent: 1, LogRetryBudget: 1},
		client: c,
		id:     Identity{ID: "r1", APIKey: "crn_run_x"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		active: map[string]context.CancelCauseFunc{},
		idle:   make(chan struct{}, 1),
		freed:  make(chan struct{}, 1),
	}
	if !a.dispatch(context.Background(), &runnerproto.PollAssignment{TraceID: "t1", RunType: "bash"}) {
		t.Fatal("the run was not started")
	}
	select {
	case <-s.manifestAsked:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never asked for its manifest")
	}

	a.Abort() // the second stop signal, while the manifest is still on its way
	a.wg.Wait()

	body := s.log("t1")
	if !strings.Contains(body, "the runner agent was stopped before this run started") {
		t.Errorf("the run's log must say that the agent was stopped before it started; got:\n%s", body)
	}
	if !strings.Contains(body, `"exitCode":1`) || !strings.Contains(body, `"endedAt"`) {
		t.Errorf("no failure envelope in the upload, so the server would not end the run:\n%s", body)
	}
}

// An agent that is stopped with no run in flight still finishes telling the
// server about a run it has just refused. The refusal is uploaded from a
// goroutine of its own, and Run returned without waiting for it: the process
// exited mid-upload and the refused run stayed `running`.
func TestAStopWaitsForARefusalBeingReported(t *testing.T) {
	s := newStopServer(t, func(n int, _ string) string {
		if n == 1 {
			// A bash run, handed over with the settings that mask bash.
			return `{"assignment":{"traceId":"t1","jobName":"j","type":"bash","scope":"","payload":{}},"control":[],"pollAfterMs":0,` +
				`"settings":{"version":1,"values":{"capabilityMask":["bash"]}}}`
		}
		return ""
	})
	s.logDelay = 400 * time.Millisecond
	stop, done := s.start(t)

	select {
	case <-s.logArrived: // the refusal is on its way, and takes 400ms to land
	case <-time.After(5 * time.Second):
		t.Fatal("the masked run was never refused")
	}
	stop() // SIGTERM with nothing in flight
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit after the stop")
	}
	if body := s.log("t1"); !strings.Contains(body, "did not start it") {
		t.Errorf("the agent exited before the refusal was uploaded; the server has:\n%q", body)
	}
}

// eventually polls cond until it holds or the time is up.
func eventually(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
