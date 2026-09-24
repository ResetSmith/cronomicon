package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// EP-8a (the expanded-panels plan) — ?offset= on the operator log
// read, the backend half of live log tailing.
//
// The contract these pin, in the order it matters:
//
//  1. offset=0 (or absent) is byte-for-byte the pre-EP-8a full read. This is
//     what makes the change additive across every existing caller.
//  2. A mid-file offset returns exactly the suffix, and X-Log-Offset names the
//     end of the file so the next poll resumes there.
//  3. An offset AT or PAST the end answers 200-empty with the real size, never
//     4xx/5xx. A log can be reaped or rotated under a live tail; an error there
//     would wedge the viewer, while "no new bytes, here is where the file
//     actually ends" lets a client notice its bookmark outlived the file and
//     restart from 0.
//  4. Garbage is rejected, not coerced to 0 — silently re-serving the whole
//     file would give a tailing client no signal that its bookmark was never
//     understood, and it would re-download the log on every poll forever.
func seedLogRun(t *testing.T, svc *Service, traceID, body string) string {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES(?, 'offsetjob', 'bash', '', 'running', 'tester', 'manual', 'ssh', datetime('now'))`, traceID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	path := mustLogPath(t, svc, traceID)
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

func getLog(t *testing.T, svc *Service, traceID, query string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/runs/" + traceID + "/log" + query
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("traceId", traceID)
	rec := httptest.NewRecorder()
	http.HandlerFunc(svc.HandleGetLog).ServeHTTP(rec, req)
	return rec
}

func TestGetLogOffset(t *testing.T) {
	svc := newTestService(t)
	const body = "line one\nline two\nline three\n"
	seedLogRun(t, svc, "offsettrace", body)
	size := strconv.Itoa(len(body))

	t.Run("no offset returns the whole log (pre-EP-8a behaviour)", func(t *testing.T) {
		rec := getLog(t, svc, "offsettrace", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		if rec.Body.String() != body {
			t.Errorf("body = %q, want the whole log %q", rec.Body.String(), body)
		}
		if got := rec.Header().Get("X-Log-Offset"); got != size {
			t.Errorf("X-Log-Offset = %q, want %q (end of file)", got, size)
		}
	})

	t.Run("offset=0 is identical to no offset", func(t *testing.T) {
		rec := getLog(t, svc, "offsettrace", "?offset=0")
		if rec.Body.String() != body {
			t.Errorf("body = %q, want %q", rec.Body.String(), body)
		}
	})

	t.Run("a mid-file offset returns exactly the suffix", func(t *testing.T) {
		cut := len("line one\n")
		rec := getLog(t, svc, "offsettrace", "?offset="+strconv.Itoa(cut))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		want := body[cut:]
		if rec.Body.String() != want {
			t.Errorf("body = %q, want the suffix %q", rec.Body.String(), want)
		}
		if got := rec.Header().Get("X-Log-Offset"); got != size {
			t.Errorf("X-Log-Offset = %q, want %q", got, size)
		}
	})

	t.Run("offset at EOF is 200-empty, the ordinary quiet poll", func(t *testing.T) {
		rec := getLog(t, svc, "offsettrace", "?offset="+size)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		if rec.Body.String() != "" {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}
		if got := rec.Header().Get("X-Log-Offset"); got != size {
			t.Errorf("X-Log-Offset = %q, want %q", got, size)
		}
	})

	t.Run("offset past EOF is 200-empty with the REAL size, never an error", func(t *testing.T) {
		// The reaped/rotated-log case: the client's bookmark outlived the file.
		// It must be able to see that its offset is beyond the end and restart.
		rec := getLog(t, svc, "offsettrace", "?offset=999999")
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (an error here wedges a live tail)", rec.Code)
		}
		if rec.Body.String() != "" {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}
		if got := rec.Header().Get("X-Log-Offset"); got != size {
			t.Errorf("X-Log-Offset = %q, want the real size %q so the client can restart", got, size)
		}
	})

	t.Run("a malformed offset is rejected, not coerced to 0", func(t *testing.T) {
		// %20 rather than a raw space: httptest.NewRequest panics on a malformed
		// URL, and the handler sees the decoded " 4" either way.
		for _, bad := range []string{"abc", "-1", "1.5", "0x10", "%204"} {
			rec := getLog(t, svc, "offsettrace", "?offset="+bad)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("offset=%q: code = %d, want 400", bad, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "invalid_offset") {
				t.Errorf("offset=%q: body = %q, want the invalid_offset code", bad, rec.Body.String())
			}
		}
	})
}

// A log file that does not exist yet (queued run, or a run that has produced no
// output) still answers 200 with X-Log-Offset: 0, so a tailing client learns
// "still zero bytes" instead of having to infer it from a missing header.
func TestGetLogMissingFileReportsZeroOffset(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		VALUES('nologtrace', 'offsetjob', 'bash', '', 'queued', 'tester', 'manual', 'ssh', datetime('now'))`); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	rec := getLog(t, svc, "nologtrace", "?offset=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Log-Offset"); got != "0" {
		t.Errorf("X-Log-Offset = %q, want \"0\"", got)
	}
}
