package api

import (
	"context"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// validateCalendarBinding applies calendar.ValidateBinding to one schedule
// entry, loading the known/global name sets from the DB.
//
// The rules themselves live in the calendar package so this path and the Git
// sync path (gitlab.validateScheduleCalendars) enforce an identical contract —
// one refusal set, both authoring surfaces. Callers that validate many entries
// in a loop pay one small query per entry; the compose paths handle a handful of
// entries per request, so the fixed-query discipline that matters for the list
// endpoints (CC.9) is not in play here.
func (s *Server) validateCalendarBinding(ctx context.Context, skip, only []string) (outSkip, outOnly []string, verr string) {
	if len(calendar.ParseNamesSlice(skip)) == 0 && len(calendar.ParseNamesSlice(only)) == 0 {
		return nil, nil, "" // the overwhelmingly common case: no bindings, no query
	}
	known, global, err := calendar.LoadFlags(ctx, s.db)
	if err != nil {
		return nil, nil, "calendar lookup failed: " + err.Error()
	}
	return calendar.ValidateBinding(skip, only, known, global)
}
