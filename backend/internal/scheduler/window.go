package scheduler

import (
	"database/sql"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

// Activation windows and schedule specs are owned by cronutil (ParseSpec
// applies the window to whichever mode an entry is in — cron, interval or
// once). This file holds only the column→value adapters the reload path needs.

// parseWindowCols builds a *Window from the nullable start_at/end_at columns.
// Unparseable values are ignored rather than fatal: a malformed bound must not
// silently widen or narrow when it can't be understood, and the validation
// layers (API 422, YAML ValidationError) already reject them at authoring time.
func parseWindowCols(startAt, endAt sql.NullString) *cronutil.Window {
	start := parseWindowBound(startAt)
	end := parseWindowBound(endAt)
	return cronutil.NewWindow(start, end)
}

func parseWindowBound(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return nil
	}
	return &t
}

// windowLogState describes a window's state for registration logging, so an
// operator reading the log can tell "not firing yet" from "misconfigured".
func windowLogState(win *cronutil.Window, now time.Time) string {
	switch {
	case win == nil:
		return ""
	case win.Pending(now):
		return "pending"
	case win.Expired(now):
		return "expired"
	default:
		return "active"
	}
}
