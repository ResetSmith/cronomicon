package runner

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// drainRequest is the optional JSON body for POST /runners/{id}/drain.
type drainRequest struct {
	TimeoutMinutes int `json:"timeoutMinutes"`
}

// HandleDrain marks a runner as draining (A6.4). Operator, CSRF required.
// POST /api/v1/runners/{id}/drain
func (s *Service) HandleDrain(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	var req drainRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.TimeoutMinutes <= 0 {
		req.TimeoutMinutes = drainDefaultMinutes
	}

	// Verify runner exists and is drainable.
	var status, capsJSON, name, inventory string
	var load int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT status, capabilities, name, load, inventory FROM runners WHERE id = ?`,
		runnerID).Scan(&status, &capsJSON, &name, &load, &inventory)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	if status == "draining" || status == "offline" {
		httpx.Fail(w, http.StatusConflict, "conflict",
			"runner is already draining or offline")
		return
	}

	ts := now()
	deadline := time.Now().UTC().
		Add(time.Duration(req.TimeoutMinutes) * time.Minute).
		Format(time.RFC3339)

	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE runners SET status = 'draining', drain_deadline_at = ?
		WHERE id = ?`, deadline, runnerID); err != nil {
		s.log.Error("set draining", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// If no active jobs, transition immediately to offline.
	if load == 0 {
		if _, err := s.db.ExecContext(r.Context(), `
			UPDATE runners SET status = 'offline', drain_deadline_at = NULL
			WHERE id = ?`, runnerID); err != nil {
			s.log.Error("immediate offline after drain", "error", err)
		} else {
			status = "offline"
		}
	} else {
		status = "draining"
	}

	// Audit entry, stamped with the same ts as the state change above.
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      sessionActor(r),
		Target:     "runner:" + name,
		RunnerName: name,
		Summary:    "drain initiated",
	})

	// Return updated runner.
	var caps []string
	_ = json.Unmarshal([]byte(capsJSON), &caps)
	httpx.JSON(w, http.StatusAccepted, runnerResponse{
		ID:           runnerID,
		Name:         name,
		Status:       status,
		Capabilities: caps,
		Load:         load,
		Inventory:    inventory,
	})
}
