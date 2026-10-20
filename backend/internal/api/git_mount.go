package api

import (
	"context"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// mountGit wires B3 (GitLab integration + schedule write-path) routes.
//
//	POST /api/v1/schedules/publish       (operator; If-Match base_sha → 412 on mismatch, A2)
//	GET  /api/v1/schedule-pushes         (operator; list push audit)
//	GET  /api/v1/git/sync                (operator; sync status)
//	POST /api/v1/git/sync                (operator; trigger resync, CSRF)
//	GET  /api/v1/git/history             (operator; paginated sync events)
//	POST /api/v1/webhooks/gitlab         (unauthenticated; X-Gitlab-Token; Global's repository)
//	POST /api/v1/webhooks/gitlab/{repoId} (the same, for the repository named)
func (s *Server) mountGit(mux *http.ServeMux) {
	// One sync service per repository, owned by a registry (GR-11). Each
	// resolves its repository's URL and token when it is built (for Global's:
	// env wins, its row otherwise, E.3), and is built again whenever the
	// connection is written, so a change needs no restart of the server.
	reg := gitlab.NewRegistry(s.db, s.log, s.cfg)
	// Reload the scheduler immediately after a git sync brings in new schedules
	// (no-op when unset, e.g. in tests). The hook self-gates on SHA change.
	if s.scheduleReload != nil {
		reg.SetOnSyncComplete(s.scheduleReload)
	}
	s.git = reg
	// The assignments' update check (C10.3) when the Notices page is opened.
	s.noticeRefresh.Extra = append(s.noticeRefresh.Extra, reg.CheckAssignmentUpdates)
	h := gitlab.NewRegistryHandlers(reg)

	// Start every repository's service, each with a first sync so the cache
	// warms at startup. They live as long as the process: at shutdown they are
	// stopped, and the drain waits for a sync in flight to unwind before the
	// pool closes.
	if err := reg.Start(s.runCtx); err != nil {
		s.log.Error("git: the repositories could not be listed; no sync service was started", "error", err)
	}
	if s.runCtx.Done() != nil {
		if s.shutdownWG != nil {
			s.shutdownWG.Add(1)
		}
		context.AfterFunc(s.runCtx, func() {
			reg.Close()
			if s.shutdownWG != nil {
				s.shutdownWG.Done()
			}
		})
	}

	// ── Webhook (unauthenticated; verified by X-Gitlab-Token header) ──────
	mux.Handle("POST /api/v1/webhooks/gitlab",
		http.HandlerFunc(h.WebhookGitLab))
	// One route per repository (GR-20). The one above is the one every
	// installation's hook points at already, and means Global's.
	mux.Handle("POST /api/v1/webhooks/gitlab/{repoId}",
		http.HandlerFunc(h.WebhookGitLab))

	// ── Schedule publish (PublishSchedule = admin OR approver, PP-B1 / closes
	// V2-9) ─────────────────────────────────────────────────────────────────
	mux.Handle("POST /api/v1/schedules/publish",
		s.requirePerm("publishSchedule", permPublishSchedule)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, ok := auth.IdentityFrom(r.Context())
				if !ok {
					httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
					return
				}
				h.PublishSchedule(id.Email, s.authorizePublish, w, r)
			}),
		),
	)

	// ── Schedule push audit (operator; GET — no CSRF) ──────────────────────
	mux.Handle("GET /api/v1/schedule-pushes",
		s.auth.RequireSession(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The publishes to the repositories of the agencies the caller
				// reads (CF-1; Global's only to everywhere-readers since 2.5.0
				// C2, question 77).
				cond, args := agencyReadFilter(r, agencyid.Owner("(SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(repo_id, 'global'))"))
				// 2.4.4: the agency the request works in, by the repository's.
				if frag, fargs := agencyctx.Narrow(r, agencyid.Owner("(SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(repo_id, 'global'))")); frag != "" {
					cond += frag
					args = append(args, fargs...)
				}
				h.ListSchedulePushes(s.db, cond, args, w, r)
			}),
		),
	)

	// ── Git sync status (operator GET — no CSRF for reads) ─────────────────
	//
	// Each of the three takes ?repo= (2.4.0, GR-9, GR-26); without it, it means
	// Global's repository, as it always did. The status and the history of a
	// repository are read by its agency's people and by those who reach every
	// agency, Global's by every session (readableRepo). A sync is started by
	// who may configure the repository: its agency's administrator, or for
	// Global's a global administrator, as before (configurableRepo).
	mux.Handle("GET /api/v1/git/sync",
		s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if repo, ok := s.readableRepo(w, r, r.URL.Query().Get("repo")); ok {
				h.GetSyncStatusOf(repo, w, r)
			}
		})))

	// ── Git sync trigger (ConfigureApp, PP-B1) ─────────────────────────────
	mux.Handle("POST /api/v1/git/sync",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if repo, ok := s.configurableRepo(w, r, r.URL.Query().Get("repo")); ok {
				h.PostSyncOf(repo, w, r)
			}
		})),
	)

	// ── Git sync history (operator GET) ───────────────────────────────────
	mux.Handle("GET /api/v1/git/history",
		s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if repo, ok := s.readableRepo(w, r, r.URL.Query().Get("repo")); ok {
				h.ListGitHistoryOf(repo, w, r)
			}
		})))

	// ── Scope resync (ConfigureApp, PP-B1) ─────────────────────────────────
	// One repository, as POST /git/sync (2.4.2, question 53): ?repo= names it,
	// Global's without it, and who may configure it starts it.
	mux.Handle("POST /api/v1/scopes/resync",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := auth.IdentityFrom(r.Context())
			if !ok {
				httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
				return
			}
			if repo, ok := s.configurableRepo(w, r, r.URL.Query().Get("repo")); ok {
				h.ResyncScopesOf(repo, id.Email, w, r)
			}
		})),
	)
}

// authorizePublish is the per-file half of the publish gate (GR-24).
//
// The route asks only whether the caller holds publishSchedule SOMEWHERE. The
// rule, by the repository the publish names:
//
//   - a global publisher (an unrestricted grant carrying the permission) may
//     publish anything, to any repository;
//   - GLOBAL's repository takes publishes from a global publisher only (the
//     owner's decision of 2026-10-13, on 2.4.0's question 19). From v2.2.2 to
//     2.3.x (GC-9) an agency's approver could publish there a job file of a
//     scope of their own; an agency now publishes to its own repository;
//   - an AGENCY's repository takes publishes from those who hold the
//     permission on that agency: its schedules and workflows as they are, and a
//     JOB file whose incoming content names a scope they hold the permission
//     on — and, when the publish REPLACES something, the scope it replaces
//     must be theirs too. "Replaces" covers both the file already at that path
//     and any job of that repository that already uses the incoming name (sync
//     upserts by name, so a second file with a taken name would rewrite the
//     first one's row). An unscoped job, or content whose scope cannot be
//     read, is a global publisher's alone.
//
// It returns "" to allow, or the 403 message, having written the audit row.
func (s *Server) authorizePublish(r *http.Request, t gitlab.PublishTarget) string {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		return "login required"
	}
	const perm = auth.PermPublishSchedule
	if id.CanUnbound(perm) {
		return ""
	}
	deny := func(where, msg string) string {
		if s.auth != nil {
			s.auth.AuditDenied(r, id.Email, "insufficient_permission", perm, auditDetails(r, msg+" ("+where+")"))
		}
		return msg
	}
	// GR-24: a repository other than Global's takes publishes from those who
	// hold publishSchedule on ITS agency. One sentence for another agency's
	// repository and for one that is not there.
	if t.Repo == "" || t.Repo == repoid.Global {
		// Whatever the file: Global's repository is the installation's.
		return deny("Global's repository", "Global's repository takes publishes only from an administrator whose "+
			"publish permission covers every agency; publish to your own agency's repository")
	}
	if t.NoSuchRepo || !id.CanAgency(perm, t.Agency) {
		return deny("repository "+t.Repo, "insufficient permissions: publishSchedule is required on the agency that repository belongs to")
	}
	if t.Kind != "job" {
		// The agency's own schedules and workflows, in the agency's own
		// repository: its publishers'. (Sync confines what they may name.)
		return ""
	}
	if !t.NewParsed || t.NewScope == "" {
		// 2.5.0 C11, defect 90: it said "an unscoped job is shared by every
		// agency", the pre-2.4 model; an agency's repository refuses one
		// (confine.go).
		return deny("no scope", "a job you publish must name a scope you hold publishSchedule on; "+
			"a job with no scope is refused in an agency's repository")
	}
	// The scope of the repository's agency (C11, defect 73): a scope's name is
	// unique within an agency only (I2), and Can matched a grant on another
	// agency's scope of the same name.
	if !id.CanIn(perm, t.Agency, t.NewScope) {
		return deny("scope "+t.NewScope, "insufficient permissions: publishSchedule is required on scope "+t.NewScope)
	}
	// The name check below is only as good as the name. Sync falls back to the
	// base name of spec.target_host for a job that carries no metadata.name, so
	// a nameless file could be aimed at any existing job's row and would skip
	// the check entirely.
	if t.Name == "" {
		return deny("no name", "a job you publish must carry metadata.name")
	}
	// One sentence for every "it is someone else's" case, so the refusal does
	// not say whether the path or the name was the one already taken.
	const taken = "this would replace a definition outside your access — choose another file name and job name"
	if t.OldExists && (!t.OldParsed || t.OldScope == "" || !id.CanIn(perm, t.Agency, t.OldScope)) {
		return deny("replaces file", taken)
	}
	{
		// Of THIS repository (2.4.0): a name is a job in each repository that
		// has one, and sync upserts by name within a repository. Asked of every
		// repository, a job of that name in another agency's repository refused
		// the publish as "outside your access", and so said the name was taken.
		rows, err := s.db.QueryContext(r.Context(),
			`SELECT COALESCE(scope,'') FROM jobs WHERE source = 'git' AND name = ? AND repo_id = ?`, t.Name, t.Repo)
		if err != nil {
			return deny("db error", "could not verify the job name; try again")
		}
		defer rows.Close()
		for rows.Next() {
			var sc string
			if err := rows.Scan(&sc); err != nil {
				return deny("db error", "could not verify the job name; try again")
			}
			if sc == "" || !id.CanIn(perm, t.Agency, sc) {
				return deny("replaces name", taken)
			}
		}
		if err := rows.Err(); err != nil {
			return deny("db error", "could not verify the job name; try again")
		}
	}
	return ""
}
