package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// A repository per agency (2.4.0 Phase R6; GR-9).
//
// Connecting, editing, testing and disconnecting a repository needs
// configureApp on ITS AGENCY. Global's is Global's, which no grant names: it
// needs a global administrator, as the /settings/gitlab routes always have
// (they still address Global's repository, and are kept for one minor).
//
// Every route here asks the same question before it reads anything, and
// answers 403 the same way whether or not the agency, or its repository, is
// there: a caller who may not configure an agency is told nothing about it.
func (s *Server) mountGitRepos(mux *http.ServeMux) {
	configure := s.requirePerm("configureApp", permConfigureApp)
	mux.Handle("GET /api/v1/git-repos", configure(http.HandlerFunc(s.handleListGitRepos)))
	mux.Handle("GET /api/v1/agencies/{agencyId}/git-repo", configure(http.HandlerFunc(s.handleGetAgencyGitRepo)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}/git-repo", configure(http.HandlerFunc(s.handlePutAgencyGitRepo)))
}

// requireRepoAgency is GR-9's gate: configureApp on the agency whose
// repository this is. It has written the 403 and the audit row when it
// returns false.
func (s *Server) requireRepoAgency(w http.ResponseWriter, r *http.Request, agencyID string) (auth.Identity, bool) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return id, false
	}
	if id.CanAgency(auth.PermConfigureApp, agencyID) {
		return id, true
	}
	msg := "you do not have configureApp on the agency this repository belongs to"
	if agencyID == agencyid.Global {
		msg = "Global's repository is shared by every agency; only a global administrator may read or change its connection"
	}
	s.denyEntityAgency(w, r, id, auth.PermConfigureApp, agencyID, msg)
	return id, false
}

// handleListGitRepos lists the repositories whose agency the caller may
// configure: a global administrator sees them all, Global's among them; an
// agency's administrator sees their agency's.
func (s *Server) handleListGitRepos(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	all, err := settings.ListRepos(r.Context(), s.db, s.cfg)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	items := make([]*settings.Repo, 0, len(all))
	for _, repo := range all {
		if id.CanAgency(auth.PermConfigureApp, repo.AgencyID) {
			items = append(items, repo)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetAgencyGitRepo(w http.ResponseWriter, r *http.Request) {
	agencyID := r.PathValue("agencyId")
	if _, ok := s.requireRepoAgency(w, r, agencyID); !ok {
		return
	}
	repoID, err := settings.RepoOfAgency(r.Context(), s.db, agencyID)
	if errors.Is(err, settings.ErrRepoNotFound) {
		httpx.Fail(w, http.StatusNotFound, "not_connected", "this agency has no repository connected")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	repo, err := settings.GetRepo(r.Context(), s.db, s.cfg, repoID)
	if errors.Is(err, settings.ErrRepoNotFound) {
		httpx.Fail(w, http.StatusNotFound, "not_connected", "this agency has no repository connected")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, repo)
}

// handlePutAgencyGitRepo writes an agency's repository's connection,
// connecting one when the agency has none (201), and starts its sync service
// from what was written, which syncs it.
func (s *Server) handlePutAgencyGitRepo(w http.ResponseWriter, r *http.Request) {
	agencyID := r.PathValue("agencyId")
	id, ok := s.requireRepoAgency(w, r, agencyID)
	if !ok {
		return
	}
	var inp settings.GitlabConfig
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	repo, created, err := settings.PutAgencyRepo(r.Context(), s.db, s.cfg, agencyID, inp, id.Email)
	switch {
	case errors.Is(err, settings.ErrNoSuchAgency):
		httpx.Fail(w, http.StatusNotFound, "not_found", "no such agency")
		return
	case errors.Is(err, settings.ErrRepoURLNotAllowed):
		httpx.Fail(w, http.StatusUnprocessableEntity, "repo_url_not_allowed", err.Error())
		return
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	case err != nil:
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	// The connection has been written: the repository's sync service is built
	// from it, and syncs (GR-11). For a repository just connected this is what
	// starts one.
	if s.git != nil {
		if err := s.git.Restart(r.Context(), repo.ID); err != nil {
			s.log.Error("the repository's connection was saved, but its sync service could not be started with it",
				"repo_id", repo.ID, "actor", id.Email, "error", err)
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, repo)
}
