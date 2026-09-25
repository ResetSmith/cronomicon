package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// ET-D — a 204 poll RETRACTS the watch set.
//
// The server sends 204 exactly when there is no assignment, no control, no
// settings AND no watches (respondControlOr204), so a 204 is a complete answer
// meaning "watch nothing", not a missing one. pollOnce's errNoWork branch
// returns before the body-bearing refresh below it ever runs, so unless that
// branch clears the specs itself, retraction-to-empty is unreachable: an idle
// agent goes on scanning a deleted job's paths forever, on every tick, with
// nothing in the UI to say why.

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// pollingAgent builds the smallest Agent that can call pollOnce against a stub
// server: a client, a logger, an identity, and a watcher. The nil settingsStore
// is deliberate — it is inert by design (TestSettingsStoreNilSafe), so the poll
// ack reads 0 without a store.
func pollingAgent(t *testing.T, h http.HandlerFunc) *Agent {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	a := &Agent{
		cfg:    Config{AllowWatch: true, WatchPaths: []string{"/data/in"}},
		client: c,
		log:    discardLogger(),
		id:     Identity{ID: "r1", APIKey: "crn_run_x"},
		active: map[string]context.CancelFunc{},
	}
	a.watcher = newWatcher(a)
	return a
}

func (wt *watcher) specCount() int {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	return len(wt.specs)
}

func (wt *watcher) pendingCount() int {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	return len(wt.pending)
}

// TestPollOnce_204ClearsTheWatchSet is the regression: the agent holds a watch
// set, the server answers 204, and the set must be empty afterwards.
func TestPollOnce_204ClearsTheWatchSet(t *testing.T) {
	a := pollingAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	spec := runnerproto.WatchSpec{JobSource: "git", JobName: "ingest", Path: "/data/in/*.csv", StableSeconds: 5}
	a.watcher.setSpecs([]runnerproto.WatchSpec{spec})
	// A file mid-stability-window for that watch, so the retraction is observed
	// to clear the pending state too — a half-settled file for a watch that no
	// longer exists must not fire later.
	a.watcher.mu.Lock()
	a.watcher.pending["/data/in/half.csv"] = pendingFile{size: 10, firstSeen: time.Now(), job: spec}
	a.watcher.mu.Unlock()

	a.pollOnce(context.Background())

	if n := a.watcher.specCount(); n != 0 {
		t.Errorf("watch specs after a 204 = %d, want 0 — the agent keeps scanning a retracted watch's paths", n)
	}
	if n := a.watcher.pendingCount(); n != 0 {
		t.Errorf("pending files after a 204 = %d, want 0 (the retracted watch's window must be dropped)", n)
	}
}

// TestPollOnce_200RefreshesTheWatchSet is the companion half: the body-bearing
// path is what INSTALLS a set, and an empty list in a 200 retracts it just as a
// 204 does. Pinned together so the two paths cannot drift into disagreeing
// about what "no watches" means.
func TestPollOnce_200RefreshesTheWatchSet(t *testing.T) {
	body := `{"watches":[{"jobSource":"git","jobName":"ingest","path":"/data/in/*.csv"}]}`
	a := pollingAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})

	a.pollOnce(context.Background())
	if n := a.watcher.specCount(); n != 1 {
		t.Fatalf("watch specs after a 200 carrying one watch = %d, want 1", n)
	}

	body = `{}`
	a.pollOnce(context.Background())
	if n := a.watcher.specCount(); n != 0 {
		t.Errorf("watch specs after a 200 carrying none = %d, want 0", n)
	}
}

// TestPollOnce_204IsSafeWithoutAWatcher: the watcher is non-nil only for an
// agent that opted in with -allow-watch, so the clearing branch must tolerate
// its absence. A nil-deref here would crash every runner that never asked for
// the feature, on its first idle poll.
func TestPollOnce_204IsSafeWithoutAWatcher(t *testing.T) {
	a := pollingAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	a.watcher = nil
	a.pollOnce(context.Background())
}
