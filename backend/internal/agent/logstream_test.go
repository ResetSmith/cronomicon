package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

func TestMakeEnvelope(t *testing.T) {
	start := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	env := makeEnvelope(7, start, end, "")
	if env.ExitCode != 7 {
		t.Errorf("exitCode = %d", env.ExitCode)
	}
	if env.DurationMs != 1500 {
		t.Errorf("durationMs = %d, want 1500", env.DurationMs)
	}
	if env.EndedAt != "2026-06-12T10:00:01Z" {
		t.Errorf("endedAt = %q", env.EndedAt)
	}
}

func TestLogBufferSealAndEnvelope(t *testing.T) {
	b := &logBuffer{}
	b.writeLine("line one")
	b.writeLine("line two")
	b.seal(runnerproto.TrailingEnvelope{ExitCode: 0, DurationMs: 10, EndedAt: "2026-06-12T10:00:00Z"})
	// Second seal is a no-op.
	b.seal(runnerproto.TrailingEnvelope{ExitCode: 99})

	pending := string(b.pending())
	lines := strings.Split(strings.TrimRight(pending, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (2 + envelope), got %d: %q", len(lines), pending)
	}
	var env runnerproto.TrailingEnvelope
	if err := json.Unmarshal([]byte(lines[2]), &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v", err)
	}
	if env.ExitCode != 0 {
		t.Errorf("second seal overwrote envelope: %+v", env)
	}
}

// fakeLogServer records the bytes it persists and can be told to drop the first
// N connections (simulating mid-stream connection loss) so we exercise resume.
type fakeLogServer struct {
	mu        sync.Mutex
	persisted []byte
	dropFirst int // drop (hang up on) this many initial requests
	attempts  int
}

func (f *fakeLogServer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.attempts++
	drop := f.dropFirst > 0
	if drop {
		f.dropFirst--
	}
	persistedLen := int64(len(f.persisted))
	f.mu.Unlock()

	// Validate X-Resume-Offset against what we have.
	if hdr := r.Header.Get("X-Resume-Offset"); hdr != "" {
		off, _ := strconv.ParseInt(hdr, 10, 64)
		if off != persistedLen {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"persistedOffset": persistedLen})
			return
		}
	}

	if drop {
		// Simulate a connection drop: hijack and close without responding.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		// Fallback: 500.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.persisted = append(f.persisted, body...)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func TestStreamLogsResumeAfterDrop(t *testing.T) {
	fs := &fakeLogServer{dropFirst: 2} // drop the first two attempts
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{ID: "r1", APIKey: "k"}

	b := &logBuffer{}
	b.writeLine("hello")
	b.writeLine("world")
	b.seal(runnerproto.TrailingEnvelope{ExitCode: 0, DurationMs: 1, EndedAt: "2026-06-12T10:00:00Z"})
	want := string(b.pending())

	if err := streamLogs(context.Background(), c, id, "trace-1", b, 5, false); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if got := string(fs.persisted); got != want {
		t.Errorf("persisted=%q want=%q", got, want)
	}
	if fs.attempts < 3 {
		t.Errorf("expected at least 3 attempts (2 drops + success), got %d", fs.attempts)
	}
}

// TestStreamLogsResumeOffsetMismatch drives the 409 persistedOffset path: the
// server already has some bytes, the agent must adopt the server's offset and
// send only the remainder.
func TestStreamLogsResumeOffsetMismatch(t *testing.T) {
	b := &logBuffer{}
	b.writeLine("aaaa")
	b.writeLine("bbbb")
	b.seal(runnerproto.TrailingEnvelope{ExitCode: 0, DurationMs: 1, EndedAt: "2026-06-12T10:00:00Z"})
	full := string(b.pending())

	// Server pre-seeds the first 5 bytes ("aaaa\n") so the agent's offset-0 POST
	// gets a 409 telling it to resume from 5.
	prefixLen := len("aaaa\n")
	fs := &fakeLogServer{persisted: []byte(full[:prefixLen])}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{ID: "r1", APIKey: "k"}

	if err := streamLogs(context.Background(), c, id, "trace-2", b, 5, false); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if got := string(fs.persisted); got != full {
		t.Errorf("persisted=%q want=%q", got, full)
	}
}
