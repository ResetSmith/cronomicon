package api

import (
	"context"
	"database/sql"
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
// Authority (LR-62). Binding needs authority over the SCOPE (requireScopeAgency)
// and a runner that already serves the scope's agency (the writer's eligibility
// check, 422 runner_not_eligible). It does not need authority over the runner.
//
// Until 2.3.0 naming a runner needed the runner's own gate, because binding
// decided which work a runner received and so counted as placing it. A runner's
// placement is now made once, by whoever owns it: an agent serves its owner, and
// the local runner serves the agencies a global administrator listed. A binding
// only narrows which of the runners already serving the agency the scope uses.
// Without this an agency could not bind its own scope to a runner that serves
// it and belongs to Global.
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
			notEligible.Error()+" — a scope takes a runner that serves its agency, and a scope that is "+
				"Global's takes a runner that serves Global; fix the runner's agency membership first")
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
// clears the binding. A runner being ADDED must serve the scope's agency; the
// writer checks that in its own transaction and names the runner if not.
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
	sc, err := settings.SetScopeRunners(r.Context(), s.db, sid, *inp.RunnerIDs, id.Email, nil)
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
// is bound to. The caller needs authority over each of those scopes, and the
// replacement must serve each one's agency (the writer's check): all or nothing.
// No authority over either runner is asked for (LR-62).
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
	// GC-6: the swap rewrites the binding of EVERY scope the old runner serves,
	// so the caller needs authority over each of those scopes. Without this an
	// administrator of one agency could re-point
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
//
// The list is filtered by the same rule as the inbox and the dismissal
// (pinVisible): configureApp on the pin's scope, a global administrator for a
// job with no scope. It was every pin for every configureApp holder, so the
// Scopes banner counted jobs of other agencies that its own "Review in Notices"
// link then did not show, and named their jobs and scopes on the way.
func (s *Server) handleListScopeBindingNotices(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	all, err := settings.ListRetiredPins(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	list := all[:0]
	for _, p := range all {
		if pinVisible(id, p.Scope) {
			list = append(list, p)
		}
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
	// A notice says a job is no longer confined to the runner its tag named.
	// Dismissing it hides that from the scope's own administrators, so it takes
	// configureApp on the notice's scope; a notice with no scope is a global
	// administrator's. A notice id that matches nothing is skipped by the writer.
	for _, nid := range inp.IDs {
		var scope sql.NullString
		err := s.db.QueryRowContext(r.Context(),
			`SELECT scope FROM retired_runner_pins WHERE id = ?`, nid).Scan(&scope)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if scope.String == "" {
			if !id.GlobalAdmin(auth.PermConfigureApp) {
				s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
					"this notice is about a job with no scope; only an administrator of every agency may dismiss it")
				return
			}
			continue
		}
		if !s.requireCan(w, r, id, auth.PermConfigureApp, scope.String) {
			return
		}
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
