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
//
// Among the entry's own agency's calendars (§0a, 2.4.0): another agency's reads
// as one that is not there.
func (s *Server) validateCalendarBinding(ctx context.Context, agency string, skip, only []string) (outSkip, outOnly []string, verr string) {
	if len(calendar.ParseNamesSlice(skip)) == 0 && len(calendar.ParseNamesSlice(only)) == 0 {
		return nil, nil, "" // the overwhelmingly common case: no bindings, no query
	}
	known, global, err := calendar.LoadFlagsFor(ctx, s.db, agency)
	if err != nil {
		return nil, nil, "calendar lookup failed: " + err.Error()
	}
	return calendar.ValidateBinding(skip, only, known, global)
}

// validateDefinitionCalendars is validateCalendarBinding for a job's or a
// workflow's own Forbid and Allow calendars (2.5.3, CA-30): the same rules
// (a name the agency lacks, a name in both, a freeze calendar as an Allow),
// said in the definition's words.
func (s *Server) validateDefinitionCalendars(ctx context.Context, agency string, forbid, allow []string) (outForbid, outAllow []string, verr string) {
	if len(calendar.ParseNamesSlice(forbid)) == 0 && len(calendar.ParseNamesSlice(allow)) == 0 {
		return nil, nil, ""
	}
	known, global, err := calendar.LoadFlagsFor(ctx, s.db, agency)
	if err != nil {
		return nil, nil, "calendar lookup failed: " + err.Error()
	}
	return calendar.ValidateDefinitionBinding(forbid, allow, known, global)
}
