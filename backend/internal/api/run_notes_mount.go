package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Run notes and acknowledgements (2.5.2 OP3; OP-8 to OP-17; migration 1650).
// What a shift knew about a run, for the next one: an append-only record of
// notes on one run, and of acknowledging it and taking that back. Anyone who
// can read the run may read and add to it (OP-13), by the run's own read check
// (requireReadableRun, requireReadableWorkflowRun), so the agency's people and
// the central team share one thread. Not narrowed by the agency header: a
// single run's read never is, so the All-agencies view acknowledges any
// agency's run. Each entry is in the run's agency's change log and Activity.
//
//	GET  /api/v1/runs/{traceId}/notes           the state and the entries, oldest first
//	POST /api/v1/runs/{traceId}/notes           {kind: note|acknowledged|unacknowledged, body}
//	GET  /api/v1/workflow-runs/{traceId}/notes  the same, for a workflow run
//	POST /api/v1/workflow-runs/{traceId}/notes
func (s *Server) mountRunNotes(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	requireCSRF := s.auth.RequireCSRF
	mux.Handle("GET /api/v1/runs/{traceId}/notes", requireSession(http.HandlerFunc(s.getRunNotes)))
	mux.Handle("POST /api/v1/runs/{traceId}/notes", requireSession(requireCSRF(http.HandlerFunc(s.postRunNote))))
	mux.Handle("GET /api/v1/workflow-runs/{traceId}/notes", requireSession(http.HandlerFunc(s.getWorkflowRunNotes)))
	mux.Handle("POST /api/v1/workflow-runs/{traceId}/notes", requireSession(requireCSRF(http.HandlerFunc(s.postWorkflowRunNote))))
}

// maxRunNote is OP-8's limit (question 199): a handoff paragraph. The job's
// annotation notes (4,096) remain for longer standing text.
const maxRunNote = 1000

// The entry kinds (OP-9).
const (
	runNoteNote   = "note"
	runNoteAck    = "acknowledged"
	runNoteUnack  = "unacknowledged"
	runNotesByRun = "run_id"
	runNotesByWR  = "workflow_run_id"
)

type runNoteEntry struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Body     string `json:"body"`
	Author   string `json:"author"`
	Relation string `json:"relation"`
	At       string `json:"at"`
}

type runAckState struct {
	By       string `json:"by"`
	Relation string `json:"relation"`
	At       string `json:"at"`
}

type runNotesResponse struct {
	Acknowledged *runAckState   `json:"acknowledged"`
	Notes        []runNoteEntry `json:"notes"`
}

// loadRunNotes is one run's record: its entries, oldest first, and its state
// (the latest acknowledged/unacknowledged entry).
func (s *Server) loadRunNotes(ctx context.Context, col, traceID string) (runNotesResponse, error) {
	out := runNotesResponse{Notes: []runNoteEntry{}}
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, body, author, actor_relation, created_at
		FROM run_notes WHERE `+col+` = ? ORDER BY id`, traceID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e runNoteEntry
		if err := rows.Scan(&e.ID, &e.Kind, &e.Body, &e.Author, &e.Relation, &e.At); err != nil {
			return out, err
		}
		out.Notes = append(out.Notes, e)
		switch e.Kind {
		case runNoteAck:
			out.Acknowledged = &runAckState{By: e.Author, Relation: e.Relation, At: e.At}
		case runNoteUnack:
			out.Acknowledged = nil
		}
	}
	return out, rows.Err()
}

func (s *Server) getRunNotes(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	if _, ok := s.requireReadableRun(w, r, traceID); !ok {
		return
	}
	s.answerRunNotes(w, r, runNotesByRun, traceID)
}

func (s *Server) getWorkflowRunNotes(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	if _, ok := s.requireReadableWorkflowRun(w, r, traceID); !ok {
		return
	}
	s.answerRunNotes(w, r, runNotesByWR, traceID)
}

func (s *Server) answerRunNotes(w http.ResponseWriter, r *http.Request, col, traceID string) {
	out, err := s.loadRunNotes(r.Context(), col, traceID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (s *Server) postRunNote(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	run, ok := s.requireReadableRun(w, r, traceID)
	if !ok {
		return
	}
	owner, _ := run["ownerAgency"].(string)
	name, _ := run["jobName"].(string)
	s.writeRunNote(w, r, runNotesByRun, traceID, agencyOrGlobal(owner), name)
}

func (s *Server) postWorkflowRunNote(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	agency, ok := s.requireReadableWorkflowRun(w, r, traceID)
	if !ok {
		return
	}
	var name string
	_ = s.db.QueryRowContext(r.Context(), `SELECT workflow_name FROM workflow_runs WHERE id = ?`, traceID).Scan(&name)
	s.writeRunNote(w, r, runNotesByWR, traceID, agencyOrGlobal(agency), name)
}

// writeRunNote appends one entry (OP-9, OP-10): a note needs text; an
// acknowledgement may carry the first note; acknowledging an acknowledged run,
// or taking back an acknowledgement that is not there, records nothing and
// answers the state as it is. The state is read and the entry written in one
// transaction, so two people pressing at once make one acknowledgement.
func (s *Server) writeRunNote(w http.ResponseWriter, r *http.Request, col, traceID, agency, target string) {
	actor, ok := s.mustActor(w, r)
	if !ok {
		return
	}
	var in struct {
		Kind string `json:"kind"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	switch {
	case in.Kind != runNoteNote && in.Kind != runNoteAck && in.Kind != runNoteUnack:
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "kind is note, acknowledged or unacknowledged")
		return
	case in.Kind == runNoteNote && in.Body == "":
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "a note needs its text")
		return
	case utf8.RuneCountInString(in.Body) > maxRunNote:
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "a note is at most 1,000 characters")
		return
	}
	ctx := r.Context()
	relation := auditlog.ActorRelation(ctx, agency)
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var last string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM run_notes WHERE `+col+` = ? AND kind <> 'note' ORDER BY id DESC LIMIT 1`,
		traceID).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	acked := last == runNoteAck
	if (in.Kind == runNoteAck && acked) || (in.Kind == runNoteUnack && !acked) {
		_ = tx.Rollback()
		s.answerRunNotes(w, r, col, traceID)
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_notes (`+col+`, kind, body, author, actor_relation, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, traceID, in.Kind, in.Body, actor, relation, now); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// OP-15: in the run's agency's change log (which keeps the text after the
	// run is pruned) and its Activity, so the agency sees Ops is on its run.
	action := map[string]string{runNoteNote: "Note added", runNoteAck: "Acknowledged", runNoteUnack: "Acknowledgement removed"}[in.Kind]
	details := "run " + traceID
	if in.Body != "" {
		details = in.Body + " (run " + traceID + ")"
	}
	_ = workflow.InsertChangeLog(ctx, s.db, agency, actor, "Runs", action, target, details)
	p := workflow.ActivityParams{Kind: "config", Actor: actor, Category: "Runs", Action: action, Target: target,
		TraceID: traceID, Summary: action + " on " + target, Details: in.Body, AgencyID: agency}
	if col == runNotesByWR {
		p.WorkflowName = target
	} else {
		p.JobName = target
	}
	_ = workflow.EmitActivity(ctx, s.db, p)
	s.answerRunNotes(w, r, col, traceID)
}

// runNoteSummary is one run's record in brief, for the lists and the overview.
type runNoteSummary struct {
	ack        *runAckState
	latestNote string
	count      int // entries with text: notes, and acknowledgements that carried one
}

// runNoteSummaries is the brief of each run named, in one read (the callers
// have drained their own cursors first: the SQLite pool rule). Runs with no
// entries are absent.
func (s *Server) runNoteSummaries(ctx context.Context, col string, ids []string) (map[string]*runNoteSummary, error) {
	out := map[string]*runNoteSummary{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+col+`, kind, body, author, actor_relation, created_at FROM run_notes
		WHERE `+col+` IN (`+strings.Repeat(",?", len(ids))[1:]+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, body, author, rel, at string
		if err := rows.Scan(&id, &kind, &body, &author, &rel, &at); err != nil {
			return nil, err
		}
		sum := out[id]
		if sum == nil {
			sum = &runNoteSummary{}
			out[id] = sum
		}
		switch kind {
		case runNoteAck:
			sum.ack = &runAckState{By: author, Relation: rel, At: at}
		case runNoteUnack:
			sum.ack = nil
		}
		if body != "" {
			sum.count++
			sum.latestNote = body
		}
	}
	return out, rows.Err()
}

// attachRunNoteSummaries adds `notes: {acknowledged, count}` to each list row
// that has entries (OP-18's History column); a row without is left as it was.
func (s *Server) attachRunNoteSummaries(ctx context.Context, col string, items []map[string]any) {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if id, _ := it["traceId"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	sums, err := s.runNoteSummaries(ctx, col, ids)
	if err != nil {
		s.log.Warn("run note summaries failed; the list renders without them", "err", err)
		return
	}
	for _, it := range items {
		id, _ := it["traceId"].(string)
		if sum := sums[id]; sum != nil {
			it["notes"] = map[string]any{"acknowledged": sum.ack != nil, "count": sum.count}
		}
	}
}
