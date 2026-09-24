package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// LU-3: the access log demotes background chatter to Debug.
//
// Why it matters: the runner long-poll fires every ~60s per runner
// (internal/agent/config.go), so a ten-runner fleet emits ~14k access lines a
// day. At Info that buries every line an operator actually needs — a failed
// trigger, a 500, an auth rejection — under noise nobody can grep past, which is
// how a log stops being read at all. Health probes are the same story at a
// smaller scale.
//
// The subtle part, and the reason for the end-to-end case below, is that the
// demotion keys on the *matched ServeMux pattern* rather than the URL path.
// r.Pattern is filled in by ServeMux during next.ServeHTTP and is therefore only
// readable after the inner handler returns; if the route were ever mounted
// somewhere the outer middleware can't see the match, isLowValueAccess would
// keep returning false and the demotion would silently stop working while every
// unit test still passed.

// capturedLog is one decoded slog JSON record. Attributes are kept untyped
// because a single buffer can also collect unrelated startup lines whose "status"
// attribute is a string (e.g. a runner status) — the record shape is not ours to
// assume, only the access line's is.
type capturedLog struct {
	Level string
	Msg   string
	Attrs map[string]any
}

func (c capturedLog) path() string {
	s, _ := c.Attrs["path"].(string)
	return s
}

func (c capturedLog) status() int {
	f, _ := c.Attrs["status"].(float64)
	return int(f)
}

// newCapturingLogger returns a logger writing JSON records into a buffer at the
// given level, plus a reader that decodes them. Asserting on log output has no
// precedent in this package; a JSON handler is the cleanest seam, since the
// emitted level is a field of the record rather than something inferred from the
// call site.
func newCapturingLogger(level slog.Level) (*slog.Logger, func(t *testing.T) []capturedLog) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	return log, func(t *testing.T) []capturedLog {
		t.Helper()
		var out []capturedLog
		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for dec.More() {
			raw := map[string]any{}
			if err := dec.Decode(&raw); err != nil {
				t.Fatalf("decode log record: %v (buffer: %s)", err, buf.String())
			}
			lvl, _ := raw["level"].(string)
			msg, _ := raw["msg"].(string)
			out = append(out, capturedLog{Level: lvl, Msg: msg, Attrs: raw})
		}
		return out
	}
}

// TestIsLowValueAccessMatchesProbesAndPollOnly pins the classification itself:
// health probes and the runner long-poll are noise, everything else is signal.
// The poll is recognised by its route pattern, never by its path, so a runner
// whose id happens to look like a route can't smuggle its requests out of the
// log — and, conversely, a real request to a runner resource still gets logged.
func TestIsLowValueAccessMatchesProbesAndPollOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		pattern string
		want    bool
	}{
		{"healthz", http.MethodGet, "/healthz", "", true},
		{"readyz", http.MethodGet, "/readyz", "", true},
		{"runner poll by pattern", http.MethodGet, "/api/v1/runners/r-123/poll", pollPattern, true},
		{"ordinary api route", http.MethodGet, "/api/v1/jobs", "GET /api/v1/jobs", false},
		{"runner detail is not the poll", http.MethodGet, "/api/v1/runners/r-123", "GET /api/v1/runners/{id}", false},
		// A poll-shaped path that did NOT match the poll route (unmatched, 404,
		// or a differently-mounted handler) stays at Info: the demotion is a
		// property of the route, not of the URL.
		{"poll-shaped path without the pattern", http.MethodGet, "/api/v1/runners/r-123/poll", "", false},
		// A runner id that mimics the pattern text must not fool the check.
		{"id spoofing the pattern", http.MethodGet, "/api/v1/runners/GET%20/api/v1/runners/%7Bid%7D/poll", "GET /api/v1/runners/{id}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Pattern = tc.pattern
			if got := isLowValueAccess(r); got != tc.want {
				t.Errorf("isLowValueAccess(%s %s, pattern=%q) = %v, want %v", tc.method, tc.path, tc.pattern, got, tc.want)
			}
		})
	}
}

// TestRequestLoggerDemotesPollThroughRealMux is the end-to-end half: it drives
// the middleware over a real ServeMux carrying the production poll pattern, so
// it proves both that ServeMux populates r.Pattern in place (the mechanism the
// demotion depends on) and that requestLogger reads it after the inner handler
// returns. A unit test on isLowValueAccess alone would keep passing if that
// propagation ever broke.
func TestRequestLoggerDemotesPollThroughRealMux(t *testing.T) {
	var seenPattern string
	mux := http.NewServeMux()
	mux.HandleFunc(pollPattern, func(w http.ResponseWriter, r *http.Request) {
		// Read inside the handler: this is where ServeMux has resolved the match.
		seenPattern = r.Pattern
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log, records := newCapturingLogger(slog.LevelDebug)
	s := &Server{log: log}
	h := s.requestLogger(mux)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/runners/r-123/poll", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if seenPattern != pollPattern {
		t.Fatalf("ServeMux left r.Pattern = %q, want %q: the pattern-based demotion cannot fire in production", seenPattern, pollPattern)
	}

	got := records(t)
	if len(got) != 3 {
		t.Fatalf("logged %d records, want 3: %+v", len(got), got)
	}
	byPath := map[string]capturedLog{}
	for _, rec := range got {
		byPath[rec.path()] = rec
	}
	for path, wantLevel := range map[string]string{
		"/api/v1/runners/r-123/poll": slog.LevelDebug.String(),
		"/healthz":                   slog.LevelDebug.String(),
		"/api/v1/jobs":               slog.LevelInfo.String(),
	} {
		rec, ok := byPath[path]
		if !ok {
			t.Errorf("no access-log record for %s", path)
			continue
		}
		if rec.Level != wantLevel {
			t.Errorf("%s logged at %s, want %s", path, rec.Level, wantLevel)
		}
		if rec.Msg != "http request" {
			t.Errorf("%s message = %q, want \"http request\"", path, rec.Msg)
		}
	}
	// The status recorder must still capture the real code on the demoted line —
	// a Debug line that lost its status would be worse than no line at all when
	// someone raises the log level to debug a poll problem.
	if s := byPath["/api/v1/runners/r-123/poll"].status(); s != http.StatusNoContent {
		t.Errorf("poll access line recorded status %d, want %d", s, http.StatusNoContent)
	}
}

// TestPollDemotionSurvivesTheRealMiddlewareChain closes the last gap between the
// unit tests above and production. requestLogger reads r.Pattern from the very
// request value it passed inward, so the demotion depends on nothing in the
// chain below it handing the mux a *copy* — r.WithContext returns a shallow
// clone, and ServeMux would then stamp Pattern onto that clone, leaving the
// outer logger's request blank and quietly restoring the 14k-lines-a-day noise.
// This drives the fully-wired Handler() so any future middleware that clones the
// request fails here rather than in someone's log volume.
func TestPollDemotionSurvivesTheRealMiddlewareChain(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "accesslog.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	log, records := newCapturingLogger(slog.LevelDebug)
	cfg := &config.Config{Addr: ":0", DevAuth: true, RunnerBootstrapToken: "boot-token"}
	s := New(Options{
		Config: cfg,
		Logger: log,
		Auth:   auth.NewService(context.Background(), cfg, pool, log),
		DB:     pool,
	})
	h := s.Handler()

	// No runner credential: the route still MATCHES (auth is per-route, inside
	// the mux), which is all the access-log demotion depends on.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/runners/r-123/poll", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	accessLines := 0
	for _, rec := range records(t) {
		if rec.Msg != "http request" {
			continue // handler-emitted lines (e.g. auth rejections) are not our concern
		}
		accessLines++
		if rec.Level != slog.LevelDebug.String() {
			t.Errorf("%s access line logged at %s through the real chain, want DEBUG: r.Pattern is not reaching requestLogger", rec.path(), rec.Level)
		}
	}
	if accessLines != 2 {
		t.Fatalf("captured %d access-log lines, want 2 (poll + healthz) — the assertion above would otherwise pass vacuously", accessLines)
	}
}

// TestRequestLoggerAtInfoDropsPollNoise is the payoff stated as an observable
// outcome: with the handler at its production Info level, a fleet's long-polls
// and probe traffic leave the access log entirely while real requests remain.
func TestRequestLoggerAtInfoDropsPollNoise(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(pollPattern, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	log, records := newCapturingLogger(slog.LevelInfo)
	h := (&Server{log: log}).requestLogger(mux)

	for range 5 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/runners/r-1/poll", nil))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))

	got := records(t)
	if len(got) != 1 {
		t.Fatalf("access log holds %d lines at Info, want 1 (only the real request): %+v", len(got), got)
	}
	if got[0].path() != "/api/v1/jobs" {
		t.Errorf("surviving line is for %s, want /api/v1/jobs", got[0].path())
	}
}
