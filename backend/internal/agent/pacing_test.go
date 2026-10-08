package agent

import (
	"context"
	"fmt"
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

// pacingServer is a server that registers the agent, answers each poll with
// whatever answer says for that poll's number (1-based), and accepts every log.
type pacingServer struct {
	mu     sync.Mutex
	polls  []string // raw query of each poll, in order
	answer func(n int, query string) string
	srv    *httptest.Server
}

func newPacingServer(t *testing.T, answer func(n int, query string) string) *pacingServer {
	t.Helper()
	p := &pacingServer{answer: answer}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/runners/register"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"runner":{"id":"r1"},"apiKey":"crn_run_x"}`))
		case strings.HasSuffix(r.URL.Path, "/poll"):
			p.mu.Lock()
			p.polls = append(p.polls, r.URL.RawQuery)
			body := p.answer(len(p.polls), r.URL.RawQuery)
			p.mu.Unlock()
			if body == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/log"):
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		default: // the manifest: the run fails at once, which frees its slot
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pacingServer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.polls)
}

// run starts an agent whose OWN poll interval is an hour, so that every poll
// after the first is one the code under test decided to make.
func (p *pacingServer) run(t *testing.T) {
	t.Helper()
	a, err := New(Config{
		ServerURL: p.srv.URL, RegistrationToken: "crn_reg_once", Name: "r", OS: "Linux",
		Capabilities: []string{"bash"}, MaxConcurrent: 5, Inventory: "cronomicon",
		IdentityFile: filepath.Join(t.TempDir(), "identity.json"),
		PollInterval: time.Hour, NoSandbox: true, LogRetryBudget: 1,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	t.Cleanup(func() {
		stop()
		a.Abort()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not exit")
		}
	})
}

func (p *pacingServer) waitForPolls(n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p.count() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// An agent that is given a run and starts it asks for the next one at once.
// The server has always said so (pollAfterMs: 0) and the agent never read it:
// it took one run per poll interval, so five queued runs on an agent with five
// free slots started a minute apart.
func TestAgentAsksAgainAtOnceAfterStartingARun(t *testing.T) {
	const queued = 3
	p := newPacingServer(t, func(n int, _ string) string {
		if n > queued {
			return "" // nothing more waiting
		}
		return fmt.Sprintf(`{"assignment":{"traceId":"t%d","jobName":"j","type":"bash","scope":"","payload":{}},"control":[],"pollAfterMs":0}`, n)
	})
	p.run(t)

	// Three runs handed out, then the poll that finds nothing: four polls, in
	// seconds, on an agent whose interval is an hour.
	if !p.waitForPolls(queued+1, 5*time.Second) {
		t.Fatalf("%d poll(s) in 5s with %d runs queued and an hour's interval: the agent waited for its next tick after being given a run", p.count(), queued)
	}
	// And it does not spin once there is nothing to take.
	time.Sleep(500 * time.Millisecond)
	if n := p.count(); n > queued+2 {
		t.Errorf("%d polls: the agent kept asking after a poll that gave it nothing", n)
	}
}

// A poll interval the server manages (2.3.2) takes effect on the poll that
// delivers it, with no restart, and is acknowledged at once.
func TestManagedPollIntervalTakesEffectWithoutARestart(t *testing.T) {
	p := newPacingServer(t, func(_ int, query string) string {
		if strings.Contains(query, "settingsVersion=1") {
			return "" // acknowledged: nothing more to say
		}
		return `{"assignment":null,"control":[],"pollAfterMs":0,"settings":{"version":1,"values":{"pollIntervalSeconds":1}}}`
	})
	p.run(t)

	// Poll 1 delivers the setting, poll 2 follows at once and acknowledges it,
	// and from then on the agent polls every second where its own interval is
	// an hour.
	if !p.waitForPolls(4, 4*time.Second) {
		t.Fatalf("%d poll(s) in 4s: a managed interval of one second was not applied", p.count())
	}
	p.mu.Lock()
	second := p.polls[1]
	p.mu.Unlock()
	if !strings.Contains(second, "settingsVersion=1") {
		t.Errorf("the poll after the settings were delivered = %q; it must acknowledge version 1", second)
	}
}

// The agent takes the interval it is sent; a store with none passes the local
// value through.
func TestSettingsStorePollInterval(t *testing.T) {
	s := &settingsStore{}
	if got := s.pollInterval(time.Minute); got != time.Minute {
		t.Errorf("no override: %s, want the local minute", got)
	}
	s.apply(&runnerproto.PollSettings{Version: 1, Values: runnerproto.PollSettingsValues{PollIntervalSeconds: new(15)}})
	if got := s.pollInterval(time.Minute); got != 15*time.Second {
		t.Errorf("managed: %s, want 15s", got)
	}
	s.apply(&runnerproto.PollSettings{Version: 2}) // the operator cleared it
	if got := s.pollInterval(time.Minute); got != time.Minute {
		t.Errorf("cleared: %s, want the local minute again", got)
	}
	var none *settingsStore
	if got := none.pollInterval(time.Minute); got != time.Minute {
		t.Errorf("nil store: %s", got)
	}
}
