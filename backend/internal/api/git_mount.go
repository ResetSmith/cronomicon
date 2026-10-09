package api

import (
	"context"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// mountGit wires B3 (GitLab integration + schedule write-path) routes.
//
//	POST /api/v1/schedules/publish       (operator; If-Match base_sha → 412 on mismatch, A2)
//	GET  /api/v1/schedule-pushes         (operator; list push audit)
//	GET  /api/v1/git/sync                (operator; sync status)
//	POST /api/v1/git/sync                (operator; trigger resync, CSRF)
//	GET  /api/v1/git/history             (operator; paginated sync events)
//	POST /api/v1/webhooks/gitlab         (unauthenticated; X-Gitlab-Token)
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
				h.ListSchedulePushes(s.db, w, r)
			}),
		),
	)

	// ── Git sync status (operator GET — no CSRF for reads) ─────────────────
	mux.Handle("GET /api/v1/git/sync",
		s.auth.RequireSession(http.HandlerFunc(h.GetSyncStatus)))

	// ── Git sync trigger (ConfigureApp, PP-B1) ─────────────────────────────
	mux.Handle("POST /api/v1/git/sync",
		s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(h.PostSync)),
	)

	// ── Git sync history (operator GET) ───────────────────────────────────
	mux.Handle("GET /api/v1/git/history",
		s.auth.RequireSession(http.HandlerFunc(h.ListGitHistory)))

	// ── Scope resync (ConfigureApp, PP-B1) ─────────────────────────────────
	mux.Handle("POST /api/v1/scopes/resync",
		s.requireGlobal(auth.PermConfigureApp)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, ok := auth.IdentityFrom(r.Context())
				if !ok {
					httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
					return
				}
				h.ResyncScopes(id.Email, w, r)
			}),
		),
	)
}

// authorizePublish is the per-file half of the publish gate (GC-9).
//
// The route asks only whether the caller holds publishSchedule SOMEWHERE. Until
// v2.2.2 nothing asked more, so an approver in one agency could write any
// `jobs/`, `schedules/` or `workflows/` file naming any scope — authoring
// another department's job through the one shared repository. The rule now:
//
//   - a global publisher (an unrestricted grant carrying the permission) may
//     publish anything, as before;
//   - anyone else may publish only a JOB file, whose incoming content names a
//     scope they hold the permission on — and, when the publish REPLACES
//     something, the scope it replaces must be theirs too. "Replaces" covers
//     both the file already at that path and any git job that already uses the
//     incoming name (sync upserts by name, so a second file with a taken name
//     would rewrite the first one's row);
//   - a schedule or workflow file, an unscoped job, or content whose scope
//     cannot be read is a global publisher's alone.
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
	if t.Kind != "job" {
		return deny(t.Kind+" file", "schedule and workflow files are shared by every agency — "+
			"only an administrator whose publish permission covers every agency may publish them")
	}
	if !t.NewParsed || t.NewScope == "" {
		return deny("no scope", "a job you publish must name a scope you hold publishSchedule on; "+
			"an unscoped job is shared by every agency")
	}
	if !id.Can(perm, t.NewScope) {
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
	if t.OldExists && (!t.OldParsed || t.OldScope == "" || !id.Can(perm, t.OldScope)) {
		return deny("replaces file", taken)
	}
	{
		rows, err := s.db.QueryContext(r.Context(),
			`SELECT COALESCE(scope,'') FROM jobs WHERE source = 'git' AND name = ?`, t.Name)
		if err != nil {
			return deny("db error", "could not verify the job name; try again")
		}
		defer rows.Close()
		for rows.Next() {
			var sc string
			if err := rows.Scan(&sc); err != nil {
				return deny("db error", "could not verify the job name; try again")
			}
			if sc == "" || !id.Can(perm, sc) {
				return deny("replaces name", taken)
			}
		}
		if err := rows.Err(); err != nil {
			return deny("db error", "could not verify the job name; try again")
		}
	}
	return ""
}
