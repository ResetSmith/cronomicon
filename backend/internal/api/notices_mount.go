package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// The notices inbox (LR-85): the one place a standing condition is shown.
//
//	GET  /api/v1/notices           (session) — the open notices the caller administers
//	POST /api/v1/notices/dismiss   (ConfigureApp + CSRF) — configureApp on each notice's agency
//
// Authority is the notice's agency: an agency's administrators (configureApp on
// it) read and dismiss its notices, Global's are a global administrator's, and
// a global administrator therefore sees them all. The list is filtered, not
// refused: a caller who administers nothing gets an empty inbox, which is the
// truth, and the shell can ask every session without a capability check first.
//
// The runner-tag pins of 2.2.0 (`retired_runner_pins`) are shown here too, read
// from their own table: they are the same kind of thing, an upgrade's leftover
// for a person to settle, and one inbox is the point. Their typed list and
// their dismissal at /scope-binding-notices are unchanged.

// noticeRefreshInterval bounds how stale the inbox can be: the checks run when
// it is opened, at most this often.
const noticeRefreshInterval = 30 * time.Second

// pinNoticePrefix marks the id of a notice that is a retired_runner_pins row.
const pinNoticePrefix = "pin:"

// kindRetiredRunnerPin is the kind a retired runner pin is listed under.
const kindRetiredRunnerPin = "retired_runner_pin"

type noticeJSON struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	AgencyID    string `json:"agencyId"`
	AgencyName  string `json:"agencyName"`
	Subject     string `json:"subject"`
	Detail      string `json:"detail"`
	FirstSeenAt string `json:"firstSeenAt"`
	LastSeenAt  string `json:"lastSeenAt"`
}

func (s *Server) mountNotices(mux *http.ServeMux) {
	s.noticeRefresh.Interval = noticeRefreshInterval
	mux.Handle("GET /api/v1/notices", s.auth.RequireSession(http.HandlerFunc(s.handleListNotices)))
	// The outer gate is "configureApp held somewhere" — the floor every write
	// route has, so a session that administers nothing is refused outright
	// rather than told it dismissed zero notices. The handler then asks for it
	// on each notice's own agency.
	mux.Handle("POST /api/v1/notices/dismiss",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDismissNotices)))
}

// pinNoticeReasons says why a pin could not become a binding, for the inbox's
// one-sentence form. The typed reason is on /scope-binding-notices.
var pinNoticeReasons = map[string]string{
	"mixed_pins":         "the scope's jobs were pinned to different tags",
	"partial_pins":       "only some of the scope's jobs were pinned",
	"no_scope":           "the job has no scope, so there is nothing to bind",
	"unknown_scope":      "the job names a scope that does not exist",
	"no_eligible_runner": "no runner both carried the tag and could serve the scope",
	"binned_job":         "the job is in the recycle bin, and restored it would not be confined",
	"leftover_git_key":   "its Git file still says runner_tag, and the scope is not bound to a runner",
}

// pinVisible is the dismiss gate of a retired pin, used for reading it here
// too: configureApp on the pin's scope, and a global administrator for a job
// with no scope.
func pinVisible(id auth.Identity, scope string) bool {
	if scope == "" {
		return id.GlobalAdmin(auth.PermConfigureApp)
	}
	return auth.ScopeReadable(id, scope) && id.Can(auth.PermConfigureApp, scope)
}

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	// A failed refresh is logged and the list is served as it stands: a notice
	// that is a little stale is more use than an inbox that will not open.
	if err := s.noticeRefresh.Refresh(r.Context(), s.db); err != nil {
		s.log.Warn("notices: checks failed; serving the list as last reconciled", "err", err)
	}
	open, err := notices.ListOpen(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	names, err := s.agencyNames(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	out := []noticeJSON{}
	for _, n := range open {
		if !id.CanAgency(auth.PermConfigureApp, n.AgencyID) {
			continue
		}
		out = append(out, noticeJSON{
			ID: n.ID, Kind: n.Kind, AgencyID: n.AgencyID, AgencyName: names[n.AgencyID],
			Subject: n.Subject, Detail: n.Detail, FirstSeenAt: n.FirstSeenAt, LastSeenAt: n.LastSeenAt,
		})
	}

	pins, err := settings.ListRetiredPins(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if len(pins) > 0 {
		scopeAgency, err := s.scopeSingleAgency(r.Context())
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		for _, p := range pins {
			if !pinVisible(id, p.Scope) {
				continue
			}
			agency := scopeAgency[p.Scope]
			if agency == "" {
				agency = agencyid.Global
			}
			where := "has no scope"
			if p.Scope != "" {
				where = "is in scope " + p.Scope
			}
			reason := pinNoticeReasons[p.Reason]
			if reason == "" {
				reason = p.Reason
			}
			out = append(out, noticeJSON{
				ID: pinNoticePrefix + strconv.FormatInt(p.ID, 10), Kind: kindRetiredRunnerPin,
				AgencyID: agency, AgencyName: names[agency], Subject: p.JobName,
				Detail: "The job " + p.JobName + " " + where + " and was pinned to runners tagged " + p.RunnerTag +
					". Tags no longer decide where a job runs, and this pin could not be turned into a scope binding: " +
					reason + ". Bind the scope to the runners that should serve it, or dismiss this if the job may run on any of its agency's runners.",
				FirstSeenAt: p.RecordedAt, LastSeenAt: p.RecordedAt,
			})
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// agencyNames maps every agency id to its name, for display.
func (s *Server) agencyNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM agencies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// scopeSingleAgency maps a scope NAME to its agency id when it has exactly one.
// A scope in several (the state Phase G2 retires) or in none maps to nothing,
// and its notices are filed under Global.
func (s *Server) scopeSingleAgency(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sc.name, MIN(sa.agency_id) FROM scopes sc
		  JOIN scope_agencies sa ON sa.scope_id = sc.id
		 GROUP BY sc.id HAVING COUNT(*) = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, agency string
		if err := rows.Scan(&name, &agency); err != nil {
			return nil, err
		}
		out[name] = agency
	}
	return out, rows.Err()
}

func (s *Server) handleDismissNotices(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// Authorize EVERY notice before dismissing any: a list that mixes the
	// caller's own with another agency's is refused whole, not half-applied.
	// An id that matches nothing is skipped, as two people may hold one list.
	var pinIDs []int64
	var own []*notices.Notice
	for _, nid := range inp.IDs {
		if rest, isPin := strings.CutPrefix(nid, pinNoticePrefix); isPin {
			n, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				continue
			}
			var scope string
			err = s.db.QueryRowContext(r.Context(),
				`SELECT COALESCE(scope, '') FROM retired_runner_pins WHERE id = ?`, n).Scan(&scope)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if !pinVisible(id, scope) {
				where := scope
				if where == "" {
					where = auth.AllScopes
				}
				s.denyEntityAgency(w, r, id, auth.PermConfigureApp, where,
					"you do not administer the scope this notice is about, so you cannot dismiss it")
				return
			}
			pinIDs = append(pinIDs, n)
			continue
		}
		n, err := notices.Get(r.Context(), s.db, nid)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if n == nil {
			continue
		}
		if !id.CanAgency(auth.PermConfigureApp, n.AgencyID) {
			s.denyEntityAgency(w, r, id, auth.PermConfigureApp, n.AgencyID,
				"you do not administer the agency this notice belongs to, so you cannot dismiss it")
			return
		}
		own = append(own, n)
	}

	dismissed := 0
	for _, n := range own {
		changed, err := notices.Dismiss(r.Context(), s.db, n.ID, id.Email)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if changed {
			dismissed++
			_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Notices", "Dismissed", n.Kind+" "+n.Subject, n.Detail)
		}
	}
	if len(pinIDs) > 0 {
		n, err := settings.DismissRetiredPins(r.Context(), s.db, pinIDs, id.Email)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		dismissed += n
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"dismissed": dismissed})
}
