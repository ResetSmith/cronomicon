package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// AN-2 (the annotations plan) — the read and write paths for operator
// annotations: a criticality flag, a contact, and free-text notes on a job or a
// workflow.
//
// The write path is the tags pair's skeleton (execution_mount.go:85,97), and
// deliberately so: annotations and tags are the same KIND of thing — operator-
// owned catalog metadata, stored only in SQLite, preserved across Git syncs —
// so they answer "who may write this" and "what happens to a binned definition"
// the same way. Where they differ is storage: tags are a column the sync upsert
// stopped writing, annotations are a sidecar the upsert never names.
//
// The gate is session + CSRF, no departmental check (AN-Q1). That matches the
// two entities being annotated: job and workflow tags are ungated for any
// logged-in user. It does NOT match every tagged entity — env-var and secret
// tags carry a departmental gate (RB-32/RF-5, settings_tags_mount.go) — so the
// argument here is the narrow one: the same page already lets any logged-in
// user tag these definitions, and splitting the models would mean answering
// "why can I edit tags but not notes" twice on one screen. Tightening this
// later is a change to two handlers.

const (
	// AN-Q3 — server-side caps. The UI mirrors them with maxLength, but a
	// maxLength is a courtesy: this is the enforcement.
	maxAnnotationNotes   = 4096
	maxAnnotationContact = 256
)

// annotation is one row of the sidecar, as read.
type annotation struct {
	critical  bool
	contact   string
	notes     string
	updatedBy string
	updatedAt string
}

// loadAnnotation reads the full annotation for one definition, by identity.
//
// Keyed on (kind, uid) and nothing else. There is no by-name fallback here or
// anywhere else that touches this table: under R2-5 two cronomicon definitions may
// share a name, so a name lookup would hand one twin the other's notes — the
// defect R2F-1 fixed for reference_bindings, which is not worth re-introducing
// for the convenience of one query.
//
// A missing row, an unreadable one and an empty one all return the zero value.
// That collapse is deliberate: an absent annotation and an empty annotation must
// be indistinguishable, which is the same invariant the all-empty-write-deletes
// rule below maintains from the other end.
func (s *Server) loadAnnotation(ctx context.Context, kind, uid string) annotation {
	if uid == "" {
		return annotation{}
	}
	var (
		a    annotation
		crit int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT critical, contact, notes, updated_by, updated_at
		  FROM annotations WHERE owner_kind = ? AND owner_uid = ?`, kind, uid).
		Scan(&crit, &a.contact, &a.notes, &a.updatedBy, &a.updatedAt)
	if err != nil {
		// sql.ErrNoRows is the common case (most definitions carry no note); a
		// real error degrades to "not annotated" rather than failing the detail
		// read, on the same display-only stance AN-4 takes: a failure here costs
		// a section, never the page.
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("annotation lookup failed; detail renders un-annotated", "kind", kind, "uid", uid, "err", err)
		}
		return annotation{}
	}
	a.critical = crit == 1
	return a
}

// annotationFields is the wire shape, EMBEDDED in both jobRow and workflowRow
// so the two twins cannot drift: one declaration, one set of JSON names, one
// place to change if a field is added.
//
// Critical has no omitempty — false is a real answer the UI acts on (it hides
// the chip), and a field that vanishes when false forces every client to
// distinguish "not critical" from "this build of the server does not know about
// criticality". The four strings do omit, since absent and empty are the same
// answer by construction (the all-empty write deletes the row).
//
// List rows carry Critical and Contact only; Notes/NotesBy/NotesAt are
// detail-only. A 4KB note per row times a page size is a lot of bytes for a
// field the catalog does not render.
type annotationFields struct {
	Critical bool   `json:"critical"`
	Contact  string `json:"contact,omitempty"`
	Notes    string `json:"notes,omitempty"`
	NotesBy  string `json:"notesBy,omitempty"`
	NotesAt  string `json:"notesAt,omitempty"`
}

// fields converts a loaded annotation to its wire shape. The zero annotation
// yields the zero annotationFields, which is exactly "un-annotated" on the wire.
func (a annotation) fields() annotationFields {
	return annotationFields{
		Critical: a.critical,
		Contact:  a.contact,
		Notes:    a.notes,
		NotesBy:  a.updatedBy,
		NotesAt:  a.updatedAt,
	}
}

// annotationInput is the PUT body: a full replace, never a patch. The client
// always holds the current value (it just rendered it), and a partial update
// would make "clear the contact" indistinguishable from "leave it alone".
type annotationInput struct {
	Critical bool   `json:"critical"`
	Contact  string `json:"contact"`
	Notes    string `json:"notes"`
}

// decodeAnnotationBody decodes and validates, trimming BEFORE measuring so a
// note that is 4096 characters plus a trailing newline is not refused for a
// character the operator cannot see.
func decodeAnnotationBody(w http.ResponseWriter, r *http.Request) (annotationInput, bool) {
	var in annotationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return in, false
	}
	in.Contact = strings.TrimSpace(in.Contact)
	// Notes keep their internal newlines (the UI renders pre-wrap, AN-Q2) — only
	// the surrounding whitespace goes.
	in.Notes = strings.TrimSpace(in.Notes)
	if len(in.Contact) > maxAnnotationContact {
		httpx.Fail(w, http.StatusBadRequest, "invalid_annotation", "contact is too long")
		return in, false
	}
	if len(in.Notes) > maxAnnotationNotes {
		httpx.Fail(w, http.StatusBadRequest, "invalid_annotation", "notes are too long")
		return in, false
	}
	return in, true
}

// resolveOwnerUID maps the route's rowid to the definition's identity.
//
// NO deleted_at predicate, deliberately (AN-Q6). Annotations follow tags, which
// are a documented exemption from the "every acting route refuses a binned
// definition" rule (FX-Q5, deleted_filter_sites_test.go): a binned definition
// keeps its annotation — migration 1060's triggers are AFTER DELETE and the bin
// is an UPDATE — so a note you can read but not correct would be the odd state.
// "Why did this get binned" is a note someone wants to write at exactly that
// moment.
//
// table is a compile-time constant chosen by the caller, never user input.
func (s *Server) resolveOwnerUID(ctx context.Context, table, rowid string) (string, bool) {
	var uid sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT uid FROM `+table+` WHERE rowid = ?`, rowid).Scan(&uid); err != nil {
		return "", false
	}
	if uid.String == "" {
		// Unreachable since 1050 made uid the primary key, but a uid-keyed write
		// that silently targets '' would write a row matching every uid-less
		// definition at once. Refuse instead.
		return "", false
	}
	return uid.String, true
}

// writeAnnotation is the shared decode → validate → resolve → upsert prefix of
// both handlers (the writeTagsUpdate shape, CC.12). On any failure it writes the
// response itself and returns ok=false; on success the caller runs its own
// re-fetch, audit and respond tail.
func (s *Server) writeAnnotation(w http.ResponseWriter, r *http.Request, kind, table, rowid, actor string) bool {
	in, ok := decodeAnnotationBody(w, r)
	if !ok {
		return false
	}
	uid, ok := s.resolveOwnerUID(r.Context(), table, rowid)
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", kind+" not found")
		return false
	}

	// An all-empty write DELETES the row rather than storing a blank one. An
	// absent annotation and an empty one have to be indistinguishable, or every
	// renderer downstream grows a second empty state ("not annotated" vs
	// "annotated with nothing, by someone, at some time") that means nothing to
	// an operator.
	if !in.Critical && in.Contact == "" && in.Notes == "" {
		if _, err := s.db.ExecContext(r.Context(),
			`DELETE FROM annotations WHERE owner_kind = ? AND owner_uid = ?`, kind, uid); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return false
		}
		return true
	}

	crit := 0
	if in.Critical {
		crit = 1
	}
	// updated_by/updated_at are server-assigned. The client never supplies them:
	// attribution an operator can set is attribution nobody can trust.
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO annotations(owner_kind, owner_uid, critical, contact, notes, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner_kind, owner_uid) DO UPDATE SET
		  critical   = excluded.critical,
		  contact    = excluded.contact,
		  notes      = excluded.notes,
		  updated_by = excluded.updated_by,
		  updated_at = excluded.updated_at`,
		kind, uid, crit, in.Contact, in.Notes, actor, time.Now().UTC().Format(time.RFC3339)); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	return true
}

// updateJobAnnotation sets a job's operator annotation (full replace).
// Addressed by integer rowid, so it is source-agnostic like the tags pair.
func (s *Server) updateJobAnnotation(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mustActor(w, r)
	if !ok {
		return
	}
	jobID := r.PathValue("jobId")
	if !s.writeAnnotation(w, r, "job", "jobs", jobID, actor) {
		return
	}
	// Re-read for the response and for the human-readable audit target
	// (rowid → name), exactly as updateJobTags does.
	jr := s.fetchJobByID(r, jobID)
	if jr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Jobs", "Updated", jr.Name, "Job annotation updated")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Jobs", Action: "Updated", Target: jr.Name,
	})
	httpx.JSON(w, http.StatusOK, jr)
}

// updateWorkflowAnnotation is the workflow twin, written at the same time on
// purpose (FX-D1: the recurring defect in this area is a change applied to one
// twin and not the other).
func (s *Server) updateWorkflowAnnotation(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mustActor(w, r)
	if !ok {
		return
	}
	wfID := r.PathValue("workflowId")
	if !s.writeAnnotation(w, r, "workflow", "workflows", wfID, actor) {
		return
	}
	wr := s.fetchWorkflowByID(r, wfID)
	if wr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
		return
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Workflows", "Updated", wr.Name, "Workflow annotation updated")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Workflows", Action: "Updated", Target: wr.Name,
	})
	httpx.JSON(w, http.StatusOK, wr)
}
