package api

import (
	"context"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
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
	// Effective repo URL + PAT: env wins, DB-backed settings otherwise (E.3).
	// Resolved once at startup — settings changes take effect on restart.
	repoURL, token := settings.ResolveGitlabRuntime(context.Background(), s.db, s.cfg)
	svc := gitlab.NewService(
		s.db,
		s.log,
		repoURL,
		token,
		gitlab.DefaultCloneDir(),
		s.cfg.WebhookSecret,
	)
	svc.Cfg = s.cfg
	// Reload the scheduler immediately after a git sync brings in new schedules
	// (no-op when unset, e.g. in tests). The hook self-gates on SHA change.
	if s.scheduleReload != nil {
		svc.SetOnSyncComplete(s.scheduleReload)
	}
	h := gitlab.NewHandlers(svc)

	// Kick off an initial background sync so the cache warms at startup.
	gitlab.StartBackgroundSync(context.Background(), svc)

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
				h.PublishSchedule(id.Email, w, r)
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
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(h.PostSync)),
	)

	// ── Git sync history (operator GET) ───────────────────────────────────
	mux.Handle("GET /api/v1/git/history",
		s.auth.RequireSession(http.HandlerFunc(h.ListGitHistory)))

	// ── Scope resync (ConfigureApp, PP-B1) ─────────────────────────────────
	mux.Handle("POST /api/v1/scopes/resync",
		s.requirePerm("configureApp", permConfigureApp)(
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
