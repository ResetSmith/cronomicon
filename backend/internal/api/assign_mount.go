package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// POST /api/v1/agencies/{agencyId}/assign — a global administrator gives a job
// or a workflow of one agency's repository to the agency in the path, as one
// commit into its repository (2.5.0 C10.1, C10.2; IN-12; gitlab/assign.go).
//
// The one create whose object's agency is the PATH's, not the agency the
// request works in: an assignment is made from the origin's side (a global
// administrator working in Global hands Global's job out), so the 2.4.4
// create rule (ownerInContext) does not apply. The change-log row is the
// target agency's (C1: the object's agency, the new copy), naming the origin.
func (s *Server) handleAssign(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body struct {
		Kind          string            `json:"kind"`
		UID           string            `json:"uid"`
		Scope         string            `json:"scope"`
		Scopes        map[string]string `json:"scopes"`
		CommitMessage string            `json:"commitMessage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if body.Kind != "job" && body.Kind != "workflow" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", `kind must be "job" or "workflow"`)
		return
	}
	if body.UID == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "uid is required: the job or workflow to assign")
		return
	}
	if s.git == nil {
		httpx.Fail(w, http.StatusServiceUnavailable, "unavailable", "the repositories' sync services are not running")
		return
	}
	target := r.PathValue("agencyId")
	res, err := s.git.Assign(r.Context(), gitlab.AssignRequest{
		Kind: body.Kind, UID: body.UID, TargetAgency: target, Scope: body.Scope, Scopes: body.Scopes,
		CommitMessage: body.CommitMessage, Actor: id.Email,
	})
	var refusal *gitlab.AssignError
	var protected *gitlab.ProtectedBranchError
	switch {
	case errors.As(err, &refusal):
		out := map[string]any{"code": refusal.Code, "message": refusal.Message}
		if len(refusal.Paths) > 0 {
			out["paths"] = refusal.Paths
		}
		if len(refusal.Calendars) > 0 {
			out["calendars"] = refusal.Calendars
		}
		if len(refusal.CatalogueCalendars) > 0 {
			out["catalogueCalendars"] = refusal.CatalogueCalendars // 2.5.3 CA-19
		}
		if len(refusal.Upstreams) > 0 {
			out["upstreams"] = refusal.Upstreams // upstream_missing (C11)
		}
		httpx.JSON(w, refusal.Status, out)
		return
	case errors.As(err, &protected):
		httpx.Fail(w, http.StatusConflict, "branch_protected", protected.Error())
		return
	case err != nil:
		httpx.Fail500(w, s.log, "assign_failed", err)
		return
	}
	action := "assigned"
	if res.Updated {
		action = "updated"
	}
	if err := settings.WriteChangeLog(r.Context(), s.db, target, id.Email, "Assignment", action,
		res.Kind+":"+res.Name, gitlab.AssignmentDetails(res)); err != nil {
		s.log.Error("assignment: change log", "error", err)
	}
	httpx.JSON(w, http.StatusCreated, res)
}
