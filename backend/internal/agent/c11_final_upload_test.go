package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// 2.5.0 C11, present defect 83: a run's final upload that spends its retry
// budget is sent again until the server takes it. Until then the agent gave up
// after about 7.5 s and forgot the run, and nothing closed it: the server
// learns a run's end only from this upload (a restart of the server longer than
// that, or its 503 while it cannot redact the run, left the run `running` for
// good).

// outageServer answers 503 to the first `down` log uploads, then persists.
type outageServer struct {
	mu        sync.Mutex
	down      int
	attempts  int
	persisted []byte
}

func (o *outageServer) handler(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts++
	if o.down > 0 {
		o.down--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(r.Body)
	o.persisted = append(o.persisted, body...)
	w.WriteHeader(http.StatusNoContent)
}

func finalUploadAgent(t *testing.T, o *outageServer, budget int) *Agent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(o.handler))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{
		cfg:    Config{ServerURL: srv.URL, LogRetryBudget: budget},
		client: c,
		id:     Identity{ID: "r1", APIKey: "k"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func sealedBuffer() (*logBuffer, string) {
	b := &logBuffer{}
	b.writeLine("done")
	b.seal(runnerproto.TrailingEnvelope{ExitCode: 0, DurationMs: 1, EndedAt: "2026-10-19T00:00:00Z"})
	return b, string(b.pending())
}

func fastResends(t *testing.T) {
	t.Helper()
	prev := finalUploadBackoff
	finalUploadBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { finalUploadBackoff = prev })
}

func TestC11_AFinalUploadOutlivesAServerOutage(t *testing.T) {
	fastResends(t)
	o := &outageServer{down: 12} // well past a budget of 2
	a := finalUploadAgent(t, o, 2)
	b, want := sealedBuffer()

	done := make(chan struct{})
	go func() { a.flushTerminal(context.Background(), "trace-1", b); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("flushTerminal did not return once the server took the upload")
	}
	if got := string(o.persisted); got != want || o.attempts != 13 {
		t.Errorf("after an outage of 12 uploads: persisted %q in %d attempts; want %q in 13", got, o.attempts, want)
	}
}

// A second stop signal (Abort, which cancels the run's context) ends the
// re-sends: the agent does not hang on a server that never comes back.
func TestC11_AbortEndsTheResendsOfAFinalUpload(t *testing.T) {
	fastResends(t)
	o := &outageServer{down: 1 << 30}
	a := finalUploadAgent(t, o, 1)
	b, _ := sealedBuffer()
	ctx, cancel := context.WithCancelCause(context.Background())

	done := make(chan struct{})
	go func() { a.flushTerminal(ctx, "trace-2", b); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel(errAgentStopped)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flushTerminal went on re-sending after the run's context ended")
	}
	if len(o.persisted) != 0 {
		t.Errorf("an upload the server refused was counted as persisted")
	}
}

// The backoff: 5 s, doubling, to a minute.
func TestC11_TheFinalUploadBacksOffToAMinute(t *testing.T) {
	for n, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute, time.Minute} {
		if got := finalUploadBackoff(n); got != want {
			t.Errorf("re-send %d waits %v, want %v", n, got, want)
		}
	}
}
