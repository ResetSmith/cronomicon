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

// refuseHarness is an agent whose server records every poll query and every
// log upload.
type refuseHarness struct {
	a     *Agent
	mu    sync.Mutex
	polls []string          // raw query of each poll
	logs  map[string]string // trace id → uploaded body
}

func newRefuseHarness(t *testing.T, maxConcurrent int) *refuseHarness {
	t.Helper()
	h := &refuseHarness{logs: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/poll"):
			h.polls = append(h.polls, r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/log"):
			body, _ := io.ReadAll(r.Body)
			trace := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/runs/"), "/log")
			h.logs[trace] += string(body)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	h.a = &Agent{
		cfg:    Config{ServerURL: srv.URL, Name: "r", MaxConcurrent: maxConcurrent, LogRetryBudget: 1},
		client: c,
		id:     Identity{ID: "r1", APIKey: "crn_run_x"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		active: map[string]context.CancelCauseFunc{},
		idle:   make(chan struct{}, 1),
		freed:  make(chan struct{}, 1),
	}
	return h
}

// An agent with no free slot must not ask for work: the claim cannot tell that
// it is full, and a run it is handed and cannot start is stranded. The same
// poll, with a slot free, asks as usual.
func TestPollOfAFullAgentClaimsNothing(t *testing.T) {
	h := newRefuseHarness(t, 1)
	claimZero := runnerproto.PollParamClaim + "=0"

	h.a.pollOnce(context.Background())
	h.a.active["t-held"] = func(error) {} // its one slot is now taken
	h.a.pollOnce(context.Background())
	delete(h.a.active, "t-held")
	h.a.pollOnce(context.Background())

	if len(h.polls) != 3 {
		t.Fatalf("polls = %q", h.polls)
	}
	if strings.Contains(h.polls[0], claimZero) || strings.Contains(h.polls[2], claimZero) {
		t.Errorf("an agent with a free slot said it would claim nothing: %q, %q", h.polls[0], h.polls[2])
	}
	if !strings.Contains(h.polls[1], claimZero) {
		t.Errorf("a full agent's poll = %q, want %s in it", h.polls[1], claimZero)
	}
}

// A run that is assigned and cannot be started is ENDED, with the reason in its
// log, for each way that can happen. Before 2.3.2 the agent logged a warning and
// dropped it, and the server went on showing the run as running.
func TestAnAssignmentThatCannotStartIsReportedAsFailed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(a *Agent)
		want  string
	}{
		{"every slot taken", func(a *Agent) { a.active["t-held"] = func(error) {} }, "as many jobs as its limit allows"},
		{"stopping", func(a *Agent) { a.draining = true }, "it is stopping and takes no new work"},
		// Abort cancels the runs it finds. An assignment on its way back from a
		// poll at that moment arrives after it, with a slot free and the drain
		// not yet begun: started, it would run behind an Abort that never
		// reaches it.
		{"told to stop a second time", func(a *Agent) { a.Abort() }, "it is stopping and takes no new work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRefuseHarness(t, 1)
			tc.setup(h.a)
			h.a.dispatch(context.Background(), &runnerproto.PollAssignment{TraceID: "t-refused", RunType: "bash"})
			h.a.wg.Wait()

			h.mu.Lock()
			body := h.logs["t-refused"]
			h.mu.Unlock()
			if !strings.Contains(body, "did not start it") || !strings.Contains(body, tc.want) {
				t.Errorf("the run's log must say that it was not started and why (%q); got:\n%s", tc.want, body)
			}
			// The trailing envelope is what moves the run out of `running`.
			if !strings.Contains(body, `"exitCode":1`) || !strings.Contains(body, `"endedAt"`) {
				t.Errorf("no failure envelope in the upload, so the server would not end the run:\n%s", body)
			}
			if _, held := h.a.active["t-refused"]; held {
				t.Error("the refused run took a slot")
			}
		})
	}
}

// A stop signal that arrives while the agent is registering must not cost it
// its enrolment. The registration token is single-use: once the server has
// answered, an agent that exits without saving its identity can only register
// again with a spent token, and is refused for good.
func TestAStopDuringRegistrationStillSavesTheIdentity(t *testing.T) {
	arrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/runners/register"):
			close(arrived)                     // the token is spent from here on
			time.Sleep(300 * time.Millisecond) // the stop signal lands in this window
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"runner":{"id":"r-new"},"apiKey":"crn_run_new"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	idFile := filepath.Join(t.TempDir(), "identity.json")
	a, err := New(Config{
		ServerURL: srv.URL, RegistrationToken: "crn_reg_once", Name: "r", OS: "Linux",
		Capabilities: []string{"bash"}, MaxConcurrent: 1, Inventory: "cronomicon",
		IdentityFile: idFile, PollInterval: 20 * time.Millisecond, NoSandbox: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never tried to register")
	}
	stop() // SIGTERM, mid-registration
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not exit after the stop")
	}
	got, err := loadIdentity(idFile)
	if err != nil || got == nil || got.ID != "r-new" || got.APIKey != "crn_run_new" {
		t.Fatalf("identity after a stop during registration = %+v (%v); the spent token's answer was thrown away", got, err)
	}
}
