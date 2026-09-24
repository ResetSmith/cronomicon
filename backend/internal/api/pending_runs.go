package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Ad-hoc run scheduling (AR) — the API side of deferred manual runs.
//
// A pending run is created by the ordinary trigger endpoints (runJob /
// triggerWorkflow) when the body carries runAt; this file owns the shared
// runAt validation and the cancel surface. Listing rides GET
// /schedules/upcoming (schedules_api.go), where pending runs appear beside the
// cron projections — one place answers "what will fire, and when".

// maxRunAtHorizon caps how far ahead a run may be scheduled. A run parked two
// years out is far more likely a typo (or an attempt at standing scheduling,
// which is what real schedules are for) than an intent.
const maxRunAtHorizon = 366 * 24 * time.Hour

// parseRunAt validates an optional deferral instant: RFC3339, strictly in the
// future, within the horizon. Returns the canonical UTC string, or "" when the
// input is empty (run immediately).
func parseRunAt(v string) (string, error) {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", errors.New("invalid runAt (want an RFC3339 timestamp, e.g. 2026-08-05T17:00:00Z)")
	}
	now := time.Now()
	if !t.After(now) {
		return "", errors.New("runAt must be in the future — omit it to run immediately")
	}
	if t.Sub(now) > maxRunAtHorizon {
		return "", errors.New("runAt is more than a year out — for standing schedules use a schedule entry instead")
	}
	return t.UTC().Format(time.RFC3339), nil
}

// mountPendingRuns wires the cancel surface. Session + CSRF, the same gate as
// triggering/killing a run: scheduling state created through the run door is
// cancellable through the run door. SU-2 scope rules apply to job rows.
func (s *Server) mountPendingRuns(mux *http.ServeMux) {
	mux.Handle("DELETE /api/v1/pending-runs/{id}",
		s.auth.RequireSession(s.auth.RequireCSRF(http.HandlerFunc(s.cancelPendingRun))))
}

func (s *Server) cancelPendingRun(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	rowID := r.PathValue("id")

	var kind, name, runAt string
	var scope sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT kind, name, run_at, scope FROM pending_runs WHERE id = ?`, rowID).
		Scan(&kind, &name, &runAt, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "pending run not found (it may have fired or been cancelled)")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// SU-2: a restricted actor may only touch pending runs whose owner scope
	// they can read — the same rule the upcoming listing applies.
	if !auth.ScopeReadable(id, scope.String) {
		s.denyScope(w, r, scope.String, "your scope grants do not cover this pending run's scope")
		return
	}

	// The promote/cancel race resolves to whoever deletes first; losing it here
	// means the run fired, which is a 404 ("it may have fired"), not an error.
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM pending_runs WHERE id = ?`, rowID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "pending run not found (it may have fired or been cancelled)")
		return
	}

	category := "Jobs"
	if kind == "workflow" {
		category = "Workflows"
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, category, "Cancelled", name,
		"Scheduled ad-hoc run for "+runAt+" cancelled")
	w.WriteHeader(http.StatusNoContent)
}
