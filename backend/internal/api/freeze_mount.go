package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// The installation-wide freeze (2.4.0, §0a, open point 2): an installation
// setting, a global administrator's, with a reason. While it is on no
// scheduled fire of any agency happens; each is recorded as skipped, with the
// reason (calendar.FreezeVerdictAt; since 2.5.2 it may carry a window, FZ-1 to
// FZ-6). It replaces the freeze that Global's global
// calendar gave every agency until calendars became their agency's.
//
//	GET /api/v1/settings/freeze  anyone signed in: whether the installation is frozen, and why
//	PUT /api/v1/settings/freeze  a global administrator: {on, reason, from?, until?}; a reason is required to
//	                             turn it on; from and until (2.5.2 FZ-1) bound it, each optional
func (s *Server) mountFreeze(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/settings/freeze", s.auth.RequireSession(http.HandlerFunc(s.getFreeze)))
	mux.Handle("PUT /api/v1/settings/freeze", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.putFreeze)))
}

// freezeView is the freeze with its state now (2.5.2 FZ-4): off, scheduled,
// in_effect or ended, so the banner and the card need no clock arithmetic.
type freezeView struct {
	calendar.Freeze
	State string `json:"state"`
}

func (s *Server) getFreeze(w http.ResponseWriter, r *http.Request) {
	f, err := calendar.ReadFreeze(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, freezeView{f, calendar.FreezeState(f, time.Now())})
}

// freezeBound reads a window bound: "" is an open end; anything else is an
// RFC 3339 time, kept in UTC.
func freezeBound(s string) (time.Time, bool) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (s *Server) putFreeze(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in struct {
		On     bool   `json:"on"`
		Reason string `json:"reason"`
		From   string `json:"from"`
		Until  string `json:"until"`
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
	// FZ-1: a window, each end optional; lifting clears it.
	var from, until time.Time
	if in.On {
		var ok bool
		if from, ok = freezeBound(in.From); !ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "from is an RFC 3339 time, or empty for now")
			return
		}
		if until, ok = freezeBound(in.Until); !ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "until is an RFC 3339 time, or empty for until lifted")
			return
		}
		if !until.IsZero() && !until.After(time.Now()) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "until is in the past: a freeze's window must end later than now")
			return
		}
		if !from.IsZero() && !until.IsZero() && !until.After(from) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "until must be after from")
			return
		}
	}
	before, _ := calendar.ReadFreeze(r.Context(), s.db)
	wasState := calendar.FreezeState(before, time.Now())
	now := time.Now().UTC().Format(time.RFC3339)
	f := calendar.Freeze{On: in.On, Reason: in.Reason, SetBy: id.Email, SetAt: now}
	if !from.IsZero() {
		f.From = from.Format(time.RFC3339)
	}
	if !until.IsZero() {
		f.Until = until.Format(time.RFC3339)
	}
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
	// FZ-6: the window is in the record when it is set; lifting is Thawed (a
	// scheduled one is cancelled); a window that simply ends records nothing.
	action, summary := "Frozen", "The installation is frozen: "+in.Reason
	if f.From != "" || f.Until != "" {
		start, end := "now", "until lifted"
		if f.From != "" {
			start = f.From
		}
		if f.Until != "" {
			end = "until " + f.Until
		}
		summary += " (from " + start + " " + end + ")"
	}
	if !in.On {
		action, summary = "Thawed", "The installation is no longer frozen"
		if wasState == calendar.FreezeScheduled {
			summary = "A scheduled freeze of the installation was cancelled before it began"
		}
	}
	s.auditCalendar(r, agencyid.Global, id.Email, action, calendar.FreezeCalendar, summary) // the installation's
	httpx.JSON(w, http.StatusOK, freezeView{f, calendar.FreezeState(f, time.Now())})
}
