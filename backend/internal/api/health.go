package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// ReadyCheck is a named readiness probe. /readyz fails if any check errors
// (T13: readiness reflects dependency + migration state).
type ReadyCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// healthz is the liveness probe — the process is up and the event loop is
// responsive. Unauthenticated (T13). It deliberately checks nothing external.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.build.Version,
		"commit":  s.build.Commit,
	})
}

// version reports the build metadata stamped into the binary (B.1).
// Unauthenticated, like the health endpoints.
func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, s.build)
}

// readyz is the readiness probe — every registered dependency check must pass.
// Unauthenticated (T13). Returns 503 with the failing component(s) so an
// orchestrator can hold traffic until migrations have applied and deps are up.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	results := make(map[string]string, len(s.readyChecks))
	ok := true
	for _, c := range s.readyChecks {
		if err := c.Check(ctx); err != nil {
			results[c.Name] = "error: " + err.Error()
			ok = false
			continue
		}
		results[c.Name] = "ok"
	}

	status := http.StatusOK
	overall := "ready"
	if !ok {
		status = http.StatusServiceUnavailable
		overall = "not_ready"
	}
	httpx.JSON(w, status, map[string]any{"status": overall, "checks": results})
}
