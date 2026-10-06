package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Scope↔runner bindings (the scope-bound-runners plan, SB band; mig. 1180).
//
// A scope may name the runners allowed to serve it. Like the agency binding it
// sits beside, this is an operator overlay on scopes of either source: set
// here, never parsed from Git, untouched by sync. What a binding means at
// dispatch is documented in internal/execspec/scopebinding.go.
//
//	PUT  /api/v1/scopes/{scopeId}/runners          (ConfigureApp + CSRF) — 422 unknown_runner, runner_not_eligible; 409 bindings_changed
//	POST /api/v1/scopes/{scopeId}/runners/preview  (ConfigureApp + CSRF) — what the PUT would change; writes nothing
//	POST /api/v1/scope-runners/replace             (ConfigureApp + CSRF) — 409 no_bindings, 422 as above
//	GET  /api/v1/scope-binding-notices             (ConfigureApp)
//	POST /api/v1/scope-binding-notices/dismiss     (ConfigureApp + CSRF)
//
// Authority. The routes carry the same coarse ConfigureApp gate as every other
// scope overlay (scopes are exempt from the departmental axis — their membership
// DEFINES the grant expansion). On top of it, NAMING a runner needs the RF-2
// runner gate for that runner: configureApp on an agency it belongs to, and
// unrestricted for a general-pool runner. Binding decides which work a runner
// receives, which is the same act as placing it. Removing a binding names no
// runner and needs nothing more than the route gate.
func (s *Server) mountScopeRunners(mux *http.ServeMux) {
	mux.Handle("PUT /api/v1/scopes/{scopeId}/runners", s.requirePerm("configureApp", permConfigureApp)(s.requireScopeAgency("scopeId", http.HandlerFunc(s.handleSetScopeRunners))))
	mux.Handle("POST /api/v1/scopes/{scopeId}/runners/preview", s.requirePerm("configureApp", permConfigureApp)(s.requireScopeAgency("scopeId", http.HandlerFunc(s.handlePreviewScopeRunners))))
	mux.Handle("POST /api/v1/scope-runners/replace", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleReplaceScopeRunner)))
	mux.Handle("GET /api/v1/scope-binding-notices", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleListScopeBindingNotices)))
	mux.Handle("POST /api/v1/scope-binding-notices/dismiss", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDismissScopeBindingNotices)))
}

// failScopeRunners maps the binding writers' errors. It reports whether it wrote
// a response.
func (s *Server) failScopeRunners(w http.ResponseWriter, err error) bool {
	var notEligible *settings.ErrRunnerNotEligible
	switch {
	case err == nil:
		return false
	case errors.Is(err, settings.ErrUnknownRunner):
		httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_runner", "the referenced runner is not registered")
	case errors.As(err, &notEligible):
		httpx.Fail(w, http.StatusUnprocessableEntity, "runner_not_eligible",
			notEligible.Error()+" — a scope in an agency takes a runner that is a member of it, and a "+
				"scope in no agency takes a general-pool runner; fix the runner's agency membership first")
	case errors.Is(err, settings.ErrBindingsChanged):
		httpx.Fail(w, http.StatusConflict, "bindings_changed",
			"this scope's bound runners changed while you were editing; reload and try again")
	case errors.Is(err, settings.ErrNoBindings):
		httpx.Fail(w, http.StatusConflict, "no_bindings", "that runner is not bound to any scope")
	default:
		httpx.Fail500(w, s.log, "update_failed", err)
	}
	return true
}

// handleSetScopeRunners replaces a scope's bound-runner set. An empty list
// clears the binding. Each runner being ADDED is authorized individually before
// the write, so a denial names the runner.
func (s *Server) handleSetScopeRunners(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	// A pointer, so an absent or null runnerIds is told apart from []. This is a
	// full replace and [] is the operation that REMOVES the restriction: a body
	// with a mistyped key must be a 422, not a silent widening that answers 200.
	var inp struct {
		RunnerIDs *[]string `json:"runnerIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if inp.RunnerIDs == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
			"runnerIds is required; send [] to clear the binding")
		return
	}
	added, _, err := settings.ScopeRunnersDelta(r.Context(), s.db, sid, *inp.RunnerIDs)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	authorized := make(map[string]bool, len(added))
	for _, rid := range added {
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp, "runner_agencies", "runner_id", rid, "runner") {
			return
		}
		authorized[rid] = true
	}
	// The writer re-derives the additions inside its own transaction and is handed
	// the set that was just authorized: if another operator unbound a runner in
	// between, that runner would now count as an addition nobody gated, and the
	// write is refused instead (409) rather than letting the race stand in for
	// the check.
	sc, err := settings.SetScopeRunners(r.Context(), s.db, sid, *inp.RunnerIDs, id.Email,
		func(runnerID string) bool { return authorized[runnerID] })
	if s.failScopeRunners(w, err) {
		return
	}
	if sc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	httpx.JSON(w, http.StatusOK, sc)
}

// handleReplaceScopeRunner swaps one runner for another on every scope the first
// is bound to. The runner being replaced is usually deregistered, so only the
// REPLACEMENT is gated — it is the one being given work.
func (s *Server) handleReplaceScopeRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp struct {
		FromRunnerID string `json:"fromRunnerId"`
		ToRunnerID   string `json:"toRunnerId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if inp.FromRunnerID == "" || inp.ToRunnerID == "" || inp.FromRunnerID == inp.ToRunnerID {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
			"fromRunnerId and toRunnerId are required and must differ")
		return
	}
	if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp, "runner_agencies", "runner_id", inp.ToRunnerID, "runner") {
		return
	}
	// GC-6: the swap rewrites the binding of EVERY scope the old runner serves,
	// so the caller needs authority over each of those scopes, not only over the
	// replacement. Without this an administrator of one agency could re-point
	// another agency's confined scope at a runner of their own.
	boundScopes, err := s.scopesBoundTo(r.Context(), inp.FromRunnerID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	for _, sid := range boundScopes {
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp, "scope_agencies", "scope_id", sid, "scope") {
			return
		}
	}
	scopes, err := settings.ReplaceScopeRunner(r.Context(), s.db, inp.FromRunnerID, inp.ToRunnerID, id.Email)
	if s.failScopeRunners(w, err) {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"scopes": scopes})
}

// scopesBoundTo lists the ids of the scopes a runner is bound to.
func (s *Server) scopesBoundTo(ctx context.Context, runnerID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope_id FROM scope_runners WHERE runner_id = ?`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// handleListScopeBindingNotices returns the runner-tag pins that could not be
// turned into a scope binding and still need an operator.
func (s *Server) handleListScopeBindingNotices(w http.ResponseWriter, r *http.Request) {
	list, err := settings.ListRetiredPins(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// handleDismissScopeBindingNotices marks notices as deliberately left alone.
func (s *Server) handleDismissScopeBindingNotices(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	n, err := settings.DismissRetiredPins(r.Context(), s.db, inp.IDs, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"dismissed": n})
}

// handlePreviewScopeRunners reports what replacing a scope's bound-runner set
// would change — which jobs move between executors, which would be refused, and
// whether the proposed runners can serve what runs on the scope — without
// changing anything. POST because it takes the proposed set as a body; it is
// read-only.
//
// It needs MORE than the write it previews. The write is a scope overlay, gated
// on ConfigureApp alone; the preview names the scope's JOBS, which the job
// routes show only to a caller who can read the scope. So the caller must be
// able to read it too — which anyone entitled to ADD a runner here can, since
// that takes configureApp on the scope's own agency.
func (s *Server) handlePreviewScopeRunners(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp struct {
		RunnerIDs *[]string `json:"runnerIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if inp.RunnerIDs == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
			"runnerIds is required; send [] to preview clearing the binding")
		return
	}
	preview, err := settings.PreviewScopeRunners(r.Context(), s.db, r.PathValue("scopeId"), *inp.RunnerIDs)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if preview == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	if !auth.ScopeReadable(id, preview.Scope) {
		httpx.Fail(w, http.StatusForbidden, "forbidden",
			"you cannot read this scope's jobs, so its binding cannot be previewed for you")
		return
	}
	httpx.JSON(w, http.StatusOK, preview)
}
