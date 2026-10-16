package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/repoid"
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
	mux.Handle("GET /api/v1/git-repo-names", s.auth.RequireSession(http.HandlerFunc(s.handleListGitRepoNames)))
	mux.Handle("GET /api/v1/agencies/{agencyId}/git-repo", configure(http.HandlerFunc(s.handleGetAgencyGitRepo)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}/git-repo", configure(http.HandlerFunc(s.handlePutAgencyGitRepo)))
	mux.Handle("GET /api/v1/git-repos/{repoId}/problems", s.auth.RequireSession(http.HandlerFunc(s.handleListRepoProblems)))
	mux.Handle("POST /api/v1/agencies/{agencyId}/git-repo/webhook-secret/rotate", configure(http.HandlerFunc(s.handleRotateAgencyWebhookSecret)))
	mux.Handle("POST /api/v1/agencies/{agencyId}/git-repo/test", configure(http.HandlerFunc(s.handleTestAgencyGitRepo)))
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
		msg = "Global's repository is Global's (§0a); only a global administrator may read or change its connection"
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

// repoName is a repository as the screens that READ one name it: which it is
// and whose. Nothing of its connection.
type repoName struct {
	ID         string `json:"id"`
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	// CanSync and CanPublish say what the caller's sync (configurableRepo) and
	// publish (authorizePublish, at the repository; a job's scope is asked at
	// the publish) would answer (2.4.2, question 55).
	CanSync    bool `json:"canSync"`
	CanPublish bool `json:"canPublish"`
}

// handleListGitRepoNames lists the repositories whose history the caller may
// read — the rule readableRepo applies to `?repo=` (GR-26): Global's for
// every session, an agency's for its people and for a grant on every agency.
// It is what History → Git sync, Schedule pushes, Scripts and Publish choose
// from (2.4.0 R8, from its review): GET /git-repos answers configureApp only,
// so an approver or a viewer had no repository to choose. Ids and agency
// names only; the connection stays behind GR-9's gate.
func (s *Server) handleListGitRepoNames(w http.ResponseWriter, r *http.Request) {
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
	items := make([]repoName, 0, len(all))
	for _, repo := range all {
		if id.ReadsAgency(repo.AgencyID) {
			publish := id.CanUnbound(auth.PermPublishSchedule) ||
				(repo.ID != repoid.Global && id.CanAgency(auth.PermPublishSchedule, repo.AgencyID))
			items = append(items, repoName{ID: repo.ID, AgencyID: repo.AgencyID, AgencyName: repo.AgencyName,
				CanSync: id.CanAgency(auth.PermConfigureApp, repo.AgencyID), CanPublish: publish})
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
	// The connection is tested before it is written (IN-2).
	repoID, rerr := settings.RepoOfAgency(r.Context(), s.db, agencyID)
	connected := rerr == nil
	switch {
	case rerr != nil && !errors.Is(rerr, settings.ErrRepoNotFound):
		httpx.Fail500(w, s.log, "db_error", rerr)
		return
	case !connected:
		var known int
		if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, agencyID).Scan(&known); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if known == 0 {
			httpx.Fail(w, http.StatusNotFound, "not_found", "no such agency")
			return
		}
		repoID = "repo-untested" // not Global's: held to the allowed hosts
	}
	if !s.requirePushableConnection(w, r, repoID, connected, inp) {
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

// readableRepo resolves the repository a READ names (GR-26; "" is Global's)
// and asks whether the caller may read it: Global's is every session's, an
// agency's is its own people's and those who reach every agency
// (Identity.ReadsAgency). A repository that is not there and one the caller
// may not read are one answer, 404, as every read gate answers.
func (s *Server) readableRepo(w http.ResponseWriter, r *http.Request, repoID string) (string, bool) {
	if repoID == "" {
		repoID = repoid.Global
	}
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return "", false
	}
	agency, err := settings.RepoAgency(r.Context(), s.db, repoID)
	if err != nil && !errors.Is(err, settings.ErrRepoNotFound) {
		httpx.Fail500(w, s.log, "db_error", err)
		return "", false
	}
	if err != nil || !id.ReadsAgency(agency) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "no such repository")
		return "", false
	}
	return repoID, true
}

// configurableRepo resolves the repository a WRITE names ("" is Global's) and
// asks GR-9's question of its agency. Everybody who may not is answered 403,
// whether or not the repository is there; a global administrator is told when
// it is not (404).
func (s *Server) configurableRepo(w http.ResponseWriter, r *http.Request, repoID string) (string, bool) {
	if repoID == "" {
		repoID = repoid.Global
	}
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return "", false
	}
	agency, err := settings.RepoAgency(r.Context(), s.db, repoID)
	if err != nil && !errors.Is(err, settings.ErrRepoNotFound) {
		httpx.Fail500(w, s.log, "db_error", err)
		return "", false
	}
	if err != nil {
		if id.GlobalAdmin(auth.PermConfigureApp) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "no such repository")
			return "", false
		}
		agency = "(unknown)"
	}
	if _, ok := s.requireRepoAgency(w, r, agency); !ok {
		return "", false
	}
	return repoID, true
}

// handleListRepoProblems lists what a repository's last sync had to say about
// its files (GR-30). Until 2.4.0 the notice was the only place they showed.
func (s *Server) handleListRepoProblems(w http.ResponseWriter, r *http.Request) {
	repoID, ok := s.readableRepo(w, r, r.PathValue("repoId"))
	if !ok {
		return
	}
	items, err := settings.ListRepoProblems(r.Context(), s.db, repoID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repoId": repoID, "items": items})
}

// handleRotateAgencyWebhookSecret rotates the webhook secret of an agency's
// repository (GR-9, GR-20: one secret set per repository). The answer carries
// the new secret, once: it is what the repository's hook is to send, at
// /api/v1/webhooks/gitlab/{the repository's id}.
func (s *Server) handleRotateAgencyWebhookSecret(w http.ResponseWriter, r *http.Request) {
	agencyID := r.PathValue("agencyId")
	id, ok := s.requireRepoAgency(w, r, agencyID)
	if !ok {
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
	s.rotateWebhookSecret(w, r, repoID, id.Email)
}

// handleTestAgencyGitRepo asks a repository's remote for its branches, with
// the connection in the request, and stores nothing (GR-9: "testing"). It is
// what a connection's form calls before it saves. A token left out, or the
// mask a read returned, means the stored one. The URL of an agency's
// repository is held to GR-10 first, so a test cannot be aimed where a
// connection could not be saved.
func (s *Server) handleTestAgencyGitRepo(w http.ResponseWriter, r *http.Request) {
	agencyID := r.PathValue("agencyId")
	if _, ok := s.requireRepoAgency(w, r, agencyID); !ok {
		return
	}
	var inp settings.GitlabConfig
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
	}
	repoID, err := settings.RepoOfAgency(r.Context(), s.db, agencyID)
	connected := err == nil
	if err != nil && !errors.Is(err, settings.ErrRepoNotFound) {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if !connected {
		// Not connected yet: the test is of what the form holds. Any id that is
		// not Global's holds the URL to the list.
		repoID = "repo-untested"
	}
	target, err := settings.ResolveRepoTest(r.Context(), s.db, s.cfg, repoID, connected, inp)
	switch {
	case errors.Is(err, settings.ErrRepoURLNotAllowed):
		httpx.Fail(w, http.StatusUnprocessableEntity, "repo_url_not_allowed", err.Error())
		return
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	case err != nil:
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	res := s.repoTest(r.Context(), s.cfg, target.URL, target.Token, target.Branch)
	httpx.JSON(w, http.StatusOK, res)
}

// requirePushableConnection tests the connection a save would write, when it
// is new or names another URL, token or branch, and answers 422 unless the
// remote answered and its token may push (§0a, IN-2; Phase I3): an
// assignment and a publish both push, and a connection that cannot is found
// out at the first of them, by whoever made it. A save that changes neither
// (the bot's name, the webhook, the expiry warning) is not asked again, so a
// remote that is down for a while does not hold those back.
func (s *Server) requirePushableConnection(w http.ResponseWriter, r *http.Request, repoID string, connected bool, inp settings.GitlabConfig) bool {
	// Global's: the URL the save stores is the one in the request (an empty
	// one clears it), and CRONOMICON_GITLAB_BASE_URL stands in for none. With
	// neither, nothing is connected and nothing is tested: a Global without a
	// repository is told so by a notice, and refused nothing (question 39).
	if repoID == repoid.Global {
		url := strings.TrimSpace(inp.RepoUrl)
		if url == "" && s.cfg != nil {
			url = strings.TrimSpace(s.cfg.GitLabBaseURL)
		}
		if url == "" {
			return true
		}
		inp.RepoUrl = url
	}
	target, err := settings.ResolveRepoTest(r.Context(), s.db, s.cfg, repoID, connected, inp)
	switch {
	case errors.Is(err, settings.ErrRepoURLNotAllowed):
		httpx.Fail(w, http.StatusUnprocessableEntity, "repo_url_not_allowed", err.Error())
		return false
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return false
	case err != nil:
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	if connected {
		stored, serr := settings.ResolveRepoTest(r.Context(), s.db, s.cfg, repoID, true, settings.GitlabConfig{})
		if serr == nil && stored.URL == target.URL && stored.Token == target.Token && stored.Branch == target.Branch {
			return true
		}
	}
	res := s.repoTest(r.Context(), s.cfg, target.URL, target.Token, target.Branch)
	if !res.OK {
		httpx.Fail(w, http.StatusUnprocessableEntity, "connection_failed",
			"the repository did not answer with this connection: "+res.Error)
		return false
	}
	if !res.CanPush {
		httpx.Fail(w, http.StatusUnprocessableEntity, "token_cannot_push",
			"the token can read the repository but may not push to it ("+res.PushError+"); "+
				"an agency's repository is written to by publishes and assignments, so its token must be able to push")
		return false
	}
	return true
}

// readableRepoSQL is GR-26 as a condition over a column that holds a row's
// repository (NULL is Global's, as it was before a repository was a row): the
// row is of Global's repository, which every session reads, or of the
// repository of an agency the caller holds a grant on. No condition for a
// caller whose grant reaches every agency. It is readableRepo for a list.
func readableRepoSQL(id auth.Identity, repoCol string) (string, []any) {
	everywhere, agencies := id.AgenciesRead()
	if everywhere {
		return "", nil
	}
	cond := " AND (COALESCE(" + repoCol + ", 'global') = 'global'"
	args := []any{}
	if len(agencies) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(agencies)), ",")
		cond += " OR " + repoCol + " IN (SELECT g.id FROM git_repos g WHERE g.agency_id IN (" + marks + "))"
		for _, a := range agencies {
			args = append(args, a)
		}
	}
	return cond + ")", args
}
