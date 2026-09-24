package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/metrics"
)

// maxRequestBody caps a single non-streaming request body (PP-M1). 2 MiB sits far
// above any job/workflow/schedule/config JSON the API accepts, yet bounds the
// allocation a hostile authenticated caller can force on the single-process
// backend — without it a multi-gigabyte POST can OOM the whole server.
const maxRequestBody = 2 << 20 // 2 MiB

// limitBody wraps the request body on mutating methods with http.MaxBytesReader
// so an oversized payload is rejected mid-read instead of buffered whole (PP-M1).
// The chunked runner log-ingest route is exempt: it legitimately streams output
// larger than the cap and bounds itself elsewhere. Long-poll is a GET (no body),
// so it needs no exemption.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if r.Body != nil && !isStreamingIngest(r) {
				r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isStreamingIngest reports whether r targets the chunked runner log-ingest
// endpoint (POST /api/v1/runs/{traceId}/log), the one body-cap exemption.
func isStreamingIngest(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, "/api/v1/runs/") &&
		strings.HasSuffix(r.URL.Path, "/log")
}

// statusRecorder captures the response status code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// pollPattern is the runner long-poll route, matched against the request's
// resolved ServeMux pattern rather than its path so a runner id can't be
// confused for the route (LU-3). Registered at runners_mount.go.
const pollPattern = "GET /api/v1/runners/{id}/poll"

// isLowValueAccess reports whether an access-log line for r is pure background
// noise that should be demoted to Debug (LU-3).
//
// Health probes are polled by k8s/docker liveness. The runner long-poll is far
// louder: at the 60s agent default (internal/agent/config.go) each runner emits
// ~1,440 lines/day, so a ten-runner fleet is ~14k lines/day and dwarfs every
// real request in the log. The poll handler itself still logs its own failures
// at Error, so demoting the access line loses no diagnostic signal.
//
// r.Pattern is populated in place by ServeMux during next.ServeHTTP, so it is
// only readable *after* the inner handler returns.
func isLowValueAccess(r *http.Request) bool {
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		return true
	}
	return r.Pattern == pollPattern
}

// requestLogger emits one structured line per request. Health probes and runner
// long-polls are logged at debug to keep the access log readable (LU-3).
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if isLowValueAccess(r) {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// metricsMiddleware records HTTP request count + latency (C.2). It reads the
// matched route pattern (r.Pattern) AFTER serving so the route label is the
// low-cardinality ServeMux pattern (e.g. "GET /api/v1/runners/{id}/poll") rather
// than the raw path with IDs. /metrics itself is skipped to avoid self-counting.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		metrics.Default().ObserveHTTP(r.Method, r.Pattern, status, time.Since(start))
	})
}

// recoverer turns a handler panic into a 500 instead of crashing the process.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered",
					"error", rec,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				httpx.Fail(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
