package api

import (
	"database/sql"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// The read surface for the file-arrival sighting ledger (FX-E1).
//
// v0.57.31 shipped the WRITE half and sold it hard: "every arrival leaves a
// sighting row … the durable answer to 'the file landed, why did nothing
// happen?', which is otherwise unanswerable." Six distinct refusal reasons were
// recorded — and not one of them was reachable by an operator. No endpoint, no
// UI, not even a retention entry, so the table also grew without bound. The
// question the feature exists to answer was answerable only by opening SQLite.
//
//	GET /api/v1/jobs/{jobId}/file-sightings   newest first, paged
//
// Read-only, session-gated, and SU-2 scope-gated like getJob itself. The first
// version claimed fetchJobByID already handled the scope and it does not — that
// helper filters nothing. That omission is why the gate is now a shared helper
// (requireReadableJob, FX2-F1) instead of a per-handler copy. Watched paths,
// file sizes and refusal reasons are operational detail a scope-restricted
// actor has no business reading, and the 404 (never a 403) keeps the route from
// serving as a cross-scope existence oracle for enumerable rowids.
func (s *Server) mountFileSightings(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/jobs/{jobId}/file-sightings",
		s.auth.RequireSession(http.HandlerFunc(s.listFileSightings)))
}

// fileSightingRow is one observed arrival and what became of it.
type fileSightingRow struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	MTime     string `json:"mtime"`
	SeenAt    string `json:"seenAt"`
	// RunnerID names who reported it first — provenance, useful when two runners
	// watch overlapping paths and an arrival seems to come from the wrong host.
	RunnerID *string `json:"runnerId"`
	// RunID is the run this arrival started; null when it was refused, in which
	// case RefusedReason says why. Exactly one of the two is normally set — a row
	// with neither is an arrival the server crashed on mid-decision.
	RunID         *string `json:"runId"`
	RefusedReason *string `json:"refusedReason"`
}

func (s *Server) listFileSightings(w http.ResponseWriter, r *http.Request) {
	// SU-2 — the shared gate (FX2-F1), which is what this route's first version
	// proved the need for by omitting the hand-copy.
	jr, ok := s.requireReadableJob(w, r, r.PathValue("jobId"))
	if !ok {
		return
	}
	page, pageSize := pageParams(r.URL.Query())

	src := jr.Source
	if src == "" {
		src = "git"
	}
	var total int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM file_watch_sightings WHERE job_source = ? AND job_name = ?`,
		src, jr.Name).Scan(&total); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, path, size_bytes, mtime, runner_id, seen_at, run_id, refused_reason
		  FROM file_watch_sightings
		 WHERE job_source = ? AND job_name = ?
		 ORDER BY seen_at DESC, id DESC
		 LIMIT ? OFFSET ?`, src, jr.Name, pageSize, (page-1)*pageSize)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	items := []fileSightingRow{}
	for rows.Next() {
		var it fileSightingRow
		var runnerID, runID, refused sql.NullString
		if err := rows.Scan(&it.ID, &it.Path, &it.SizeBytes, &it.MTime,
			&runnerID, &it.SeenAt, &runID, &refused); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if runnerID.Valid {
			it.RunnerID = &runnerID.String
		}
		if runID.Valid && runID.String != "" {
			it.RunID = &runID.String
		}
		if refused.Valid && refused.String != "" {
			it.RefusedReason = &refused.String
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}
