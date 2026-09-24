// Why this file exists (LU-12).
//
// A trace ID arrives as an unvalidated path parameter and is concatenated
// straight into a filesystem path — filepath.Join(logDir, traceID+".log") — at
// three separate sites (runner ingest, runner read, the SSH test-run writer).
// Nothing else in the codebase checks its shape: openapi's `format: uuid` is
// documentation, not enforcement, and the ServeMux `{traceId}` wildcard being
// single-segment is a property of today's routing rather than a guarantee the
// filename builder is entitled to rely on. filepath.Join CLEANS its result, so
// a "../" component silently resolves to a real path outside the log directory
// instead of erroring.
//
// So this pins two things: the predicate itself (including the short literal IDs
// the rest of the suite depends on, which a UUID-only regex would break), and
// that the HTTP handlers actually reject a traversal-shaped id with 400 rather
// than writing or reading a file outside the log tree.
package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestValidTraceIDAcceptsRealIDsAndRejectsPathTricks is the predicate table. The
// accept half matters as much as the reject half: the guard sits on the live
// ingest path, so a rule that is tighter than the IDs actually in use would take
// production run logs offline, and the short literals below are the shapes the
// existing suite (and LU-Q9(c)'s "<code>-<uuidv7>" form) relies on.
func TestValidTraceIDAcceptsRealIDsAndRejectsPathTricks(t *testing.T) {
	realUUID := db.NewTraceID()

	for _, tc := range []struct {
		name string
		id   string
		want bool
	}{
		// Accepted: what production and the test suite actually mint.
		{"uuidv7 as minted by db.NewTraceID", realUUID, true},
		{"short literal run-1", "run-1", true},
		{"short literal r1", "r1", true},
		{"short literal run-bash", "run-bash", true},
		{"underscore and dot inside", "run_1.2", true},
		{"64 chars is the boundary and is allowed", strings.Repeat("a", 64), true},

		// Rejected: anything that could name a file outside the log directory,
		// or that is not a usable filename component at all.
		{"empty", "", false},
		{"bare dot-dot", "..", false},
		{"classic traversal", "../../etc/passwd", false},
		{"forward slash", "a/b", false},
		{"backslash (Windows separator, and a filename oddity everywhere else)", `a\b`, false},
		{"leading dot hides the file and is never a real trace id", ".hidden", false},
		{"dot-dot embedded mid-string", "run..1", false},
		{"embedded NUL truncates the path at the syscall boundary", "run\x001", false},
		{"65 chars is one past the bound", strings.Repeat("a", 65), false},
		{"absolute path", "/etc/passwd", false},
		{"URL-ish component", "a:b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidTraceID(tc.id); got != tc.want {
				t.Errorf("ValidTraceID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestLogPathRejectsTraversalTraceID is the seam between the predicate and the
// filename: logPath must refuse rather than hand back a cleaned path that has
// already escaped. The assertion on the escaped path is the point — filepath.Join
// would have produced a perfectly valid path pointing at the parent directory.
func TestLogPathRejectsTraversalTraceID(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.logPath("", "../escaped"); err == nil {
		t.Fatal("logPath(\"../escaped\") returned no error; it must reject a traversing id")
	} else if err != ErrBadTraceID {
		t.Errorf("logPath error = %v, want ErrBadTraceID", err)
	}
	// A good id still resolves inside the log directory.
	good, err := svc.logPath("", "run-1")
	if err != nil {
		t.Fatalf("logPath(\"run-1\"): %v", err)
	}
	if want := filepath.Join(svc.LogDir(), "run-1.log"); good != want {
		t.Errorf("logPath = %q, want %q", good, want)
	}
}

// TestLogEndpointsRejectMalformedTraceIDWith400 drives the real handlers and
// pins the distinction that was silently wrong: a MALFORMED id is 400, a
// well-formed id that simply has no run is 404.
//
// That ordering is the whole point. Both handlers look the run up before doing
// anything with the path, so a guard placed only where the filename is built is
// dead code — every malformed id 404s on the missing row first and the 400 is
// never reachable. Asserting the two codes separately is what keeps the check at
// the top of the handler; a unit test on ValidTraceID alone would not notice it
// sliding back down.
//
// No run rows are inserted for the malformed ids on purpose: they must be
// rejected before the existence lookup, so a 404 here is a failure.
func TestLogEndpointsRejectMalformedTraceIDWith400(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	// Nest the log dir one level down so a "../" escape would land inside the
	// test's own temp tree, where its absence can actually be asserted.
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	svc.SetLogDir(logDir)
	escaped := filepath.Join(root, "escaped.log")

	runnerID, tok := "runner-traceid", "amt_run_traceid"
	insertRunner(t, svc, runnerID, "traceid", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)

	ingest := as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog))
	read := http.HandlerFunc(svc.HandleGetLog)

	// A "../" segment is normalised away by any real HTTP client before it can
	// reach the router, so the traversal shapes are covered by the predicate test
	// above; these are the malformed shapes that DO survive a real request, plus
	// the traversal forms exercised at the handler seam for completeness.
	for _, bad := range []string{
		".hidden",               // leading dot: hides the file, never a real id
		strings.Repeat("z", 70), // past the 64-char filename bound
		"bad id",                // a space — decoded from bad%20id on the wire
		"a/b",                   // separator
		"../escaped",            // traversal, at the handler seam
		"../../etc/passwd",      // the classic
	} {
		t.Run("POST "+bad, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/x/log", strings.NewReader("pwned\n"))
			req.Header.Set("Authorization", "Bearer "+tok)
			req.SetPathValue("traceId", bad)
			rec := httptest.NewRecorder()
			ingest.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("ingest with traceId %q = %d, want 400; body: %s", bad, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "invalid_trace_id") {
				t.Errorf("ingest with traceId %q body = %s, want code invalid_trace_id", bad, rec.Body.String())
			}
		})

		t.Run("GET "+bad, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/log", nil)
			req.SetPathValue("traceId", bad)
			rec := httptest.NewRecorder()
			read.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("read with traceId %q = %d, want 400; body: %s", bad, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "invalid_trace_id") {
				t.Errorf("read with traceId %q body = %s, want code invalid_trace_id", bad, rec.Body.String())
			}
		})
	}

	// The other half of the distinction: a perfectly well-formed id with no run
	// behind it is still 404. If the guard were ever widened to reject real trace
	// IDs, this is the assertion that would catch it before production does.
	missing := db.NewTraceID()
	t.Run("well-formed but nonexistent stays 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/x/log", strings.NewReader("data\n"))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.SetPathValue("traceId", missing)
		rec := httptest.NewRecorder()
		ingest.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("ingest with unknown-but-valid id = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}

		req = httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/log", nil)
		req.SetPathValue("traceId", missing)
		rec = httptest.NewRecorder()
		read.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("read with unknown-but-valid id = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
	})

	// Nothing was written outside the log directory…
	if _, err := os.Stat(escaped); err == nil {
		t.Errorf("ingest wrote %s — a traversing trace id escaped the log directory", escaped)
	}
	// …and the log directory itself stayed empty, so every rejection happened
	// before any file was created under a cleaned-but-wrong name.
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("readdir log dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("log dir contains %v after rejected requests, want nothing", entries)
	}
}
