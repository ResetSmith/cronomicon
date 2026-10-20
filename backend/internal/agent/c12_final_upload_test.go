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
)

// 2.5.0 C12, present defect 92: a run's final upload is sent again only when
// the failure may clear. Cronomicon's own refusal with 413 (the run's log at its
// cap, on a server older than C12), 401 (a deregister revoked the runner's key)
// or 404 (no such run for this runner) is its answer for good; C11's re-sends
// sent them every minute for as long as the agent lived, holding the run's
// slot, so a full agent was handed nothing and a stop never drained.
func TestC12_TheServersRefusalOfAFinalUploadIsNotSentAgain(t *testing.T) {
	fastResends(t)
	for _, code := range []int{http.StatusRequestEntityTooLarge, http.StatusUnauthorized, http.StatusNotFound, http.StatusBadRequest} {
		s := &statusServer{code: code, body: `{"code":"refused","message":"no"}`}
		a := statusUploadAgent(t, s, 2)
		b, _ := sealedBuffer()
		done := make(chan struct{})
		go func() { a.flushTerminal(context.Background(), "trace-p", b); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d: flushTerminal still re-sending after 5 s", code)
		}
		if n := s.count(); n != 2 {
			t.Errorf("%d: %d uploads, want the budget's 2 and no re-send", code, n)
		}
	}
}

// A proxy answering in the server's place is not the server's answer: Traefik
// says "404 page not found" while the server's container is stopped (an
// upgrade), and the end must still arrive once the server is back.
func TestC12_AProxysRefusalOfAFinalUploadIsSentAgain(t *testing.T) {
	fastResends(t)
	s := &statusServer{code: http.StatusNotFound, body: "404 page not found", untilOK: 6}
	a := statusUploadAgent(t, s, 2)
	b, _ := sealedBuffer()
	done := make(chan struct{})
	go func() { a.flushTerminal(context.Background(), "trace-x", b); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("flushTerminal did not return once the server took the upload")
	}
	if n := s.count(); n != 7 {
		t.Errorf("%d uploads, want 6 answered by the proxy and the 7th taken", n)
	}
}

// What may clear is still sent again: a 5xx and 429, the server's or not, and
// a transport failure.
func TestC12_ATransientRefusalOfAFinalUploadIsSentAgain(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusTooManyRequests} {
		for _, refusal := range []bool{true, false} {
			if !finalUploadRetryable(&logStatusError{code: code, refusal: refusal}) {
				t.Errorf("%d (server's: %v): not retried", code, refusal)
			}
		}
	}
	if !finalUploadRetryable(context.DeadlineExceeded) {
		t.Errorf("a transport failure is not retried")
	}
	for body, want := range map[string]bool{
		`{"code":"not_found","message":"run not found"}`: true,
		"404 page not found":                             false,
		`{"message":"no code"}`:                          false,
		"":                                               false,
	} {
		if got := isServerRefusal(body); got != want {
			t.Errorf("isServerRefusal(%q) = %v, want %v", body, got, want)
		}
	}
}

// statusServer answers every log upload with code and body; with untilOK set,
// only the first untilOK, and 204 after.
type statusServer struct {
	code    int
	body    string
	untilOK int

	mu sync.Mutex
	n  int
}

func (s *statusServer) handler(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	if s.untilOK > 0 && n > s.untilOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(s.code)
	_, _ = io.WriteString(w, s.body)
}

func (s *statusServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func statusUploadAgent(t *testing.T, s *statusServer, budget int) *Agent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
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
