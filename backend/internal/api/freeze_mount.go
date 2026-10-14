package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// The installation-wide freeze (2.4.0, §0a, open point 2): an installation
// setting, a global administrator's, with a reason. While it is on no
// scheduled fire of any agency happens; each is recorded as skipped, with the
// reason (calendar.FreezeVerdict). It replaces the freeze that Global's global
// calendar gave every agency until calendars became their agency's.
//
//	GET /api/v1/settings/freeze  anyone signed in: whether the installation is frozen, and why
//	PUT /api/v1/settings/freeze  a global administrator: {on, reason}; a reason is required to turn it on
func (s *Server) mountFreeze(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/settings/freeze", s.auth.RequireSession(http.HandlerFunc(s.getFreeze)))
	mux.Handle("PUT /api/v1/settings/freeze", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.putFreeze)))
}

func (s *Server) getFreeze(w http.ResponseWriter, r *http.Request) {
	f, err := calendar.ReadFreeze(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, f)
}

func (s *Server) putFreeze(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in struct {
		On     bool   `json:"on"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.On && in.Reason == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "a freeze of the installation takes a reason: it is what every skipped fire records")
		return
	}
	if len(in.Reason) > 500 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "the reason is longer than 500 characters")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	f := calendar.Freeze{On: in.On, Reason: in.Reason, SetBy: id.Email, SetAt: now}
	raw, err := json.Marshal(f)
	if err != nil {
		httpx.Fail500(w, s.log, "encode", err)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, last_modified_by = excluded.last_modified_by,
		                               last_modified_at = excluded.last_modified_at`,
		calendar.FreezeKey, string(raw), id.Email, now); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	action, summary := "Frozen", "The installation is frozen: "+in.Reason
	if !in.On {
		action, summary = "Thawed", "The installation is no longer frozen"
	}
	s.auditCalendar(r, id.Email, action, calendar.FreezeCalendar, summary)
	httpx.JSON(w, http.StatusOK, f)
}
