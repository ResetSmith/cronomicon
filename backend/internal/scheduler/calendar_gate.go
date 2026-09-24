package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// The calendar gate (the calendar-update plan, CAL-6/CAL-7).
//
// Suppression is decided at FIRE TIME, beside the existing isPaused check,
// rather than by decorating the cron schedule. §2.3 argues the choice; the two
// consequences that shape this file are:
//
//   - Bindings are read FRESH PER FIRE, following the TG-1 precedent, so editing
//     a calendar or a binding takes effect on the next fire with no scheduler
//     reload and no dbFingerprint change. Reference data must never be able to
//     trigger a rebuild of every cron entry.
//   - A suppressed fire is RECORDED, not dropped. The whole point of the feature
//     is answering "prove this job did not run on the holiday, deliberately".
//
// cronutil is untouched: the timing wheel stays unaware calendars exist.

// location returns the zone the engine currently fires in, guarded by the same
// mutex RebuildWithLocation swaps it under (CAL-7). The day a fire lands on MUST
// be computed here and never in UTC — see calendar.DayOf.
func (s *Scheduler) location() *time.Location {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loc == nil {
		return time.Local
	}
	return s.loc
}

// calendarVerdict resolves whether this fire is suppressed by a calendar.
//
// ownerKind is "job" or "workflow"; the bindings live on the runtime
// definition_schedules row, which a ref expansion has already copied them down
// onto, so one row answers the question.
//
// # Failure is OPEN, deliberately
//
// A DB error reading the bindings or the calendars yields "not suppressed" plus
// a loud log line. The alternative — failing closed — would convert a transient
// SQLite hiccup into a silent, fleet-wide halt of all scheduled automation, with
// no human watching and nothing in History to say why. An unsuppressed fire on a
// holiday is a visible, correctable policy violation; a fleet that quietly stops
// firing is neither.
//
// Note this extends §2.2, which legislates fail-CLOSED for only-polarity: on a
// DB error an only-bound entry fires rather than staying silent. That rule is
// about a calendar that resolves to no days, not about a resolver that could not
// run at all; a database this scheduler cannot read is not a policy statement.
func (s *Scheduler) calendarVerdict(ctx context.Context, source, ownerKind, ownerName, scheduleName string) calGate {
	loc := s.location()
	day := calendar.DayOf(time.Now(), loc)
	gate := calGate{Day: day, Loc: loc}

	skip, only := s.calendarBindings(ctx, source, ownerKind, ownerName, scheduleName)

	// NOTE: this read happens even when the entry names no calendars, and that is
	// not waste — the global tier (§2.8) unions a change-freeze calendar's days
	// into EVERY entry's skip set, so an entry with no bindings of its own is
	// still suppressible. There is no empty-bindings fast path to take.
	sets, err := calendar.Load(ctx, s.db, append(append([]string{}, skip...), only...))
	if err != nil {
		s.log.Error("scheduler: load calendars — firing without calendar suppression",
			"owner", ownerName, "kind", ownerKind, "schedule", scheduleName, "err", err)
		return gate
	}

	gate.Verdict = calendar.Evaluate(sets, skip, only, day)
	return gate
}

// calGate carries the verdict together with the day and zone it was decided in,
// so the audit record de-dupes against exactly the day the decision used rather
// than re-deriving it from a clock that may have crossed midnight in between.
type calGate struct {
	Verdict calendar.Verdict
	Day     string
	Loc     *time.Location
}

// Suppressed reports whether this fire must not happen.
func (g calGate) Suppressed() bool { return g.Verdict.Suppressed }

// record builds the audit record for a suppressed fire.
func (g calGate) record() skipRecord {
	return skipRecord{
		Reason:   calendar.Reason(g.Verdict),
		Mode:     dedupeDay,
		Calendar: g.Verdict.Calendar,
		Day:      g.Day,
		Loc:      g.Loc,
	}
}

// calendarBindings reads one entry's skip/only calendar names from the runtime
// schedule row. A missing row (an entry that vanished between registration and
// this fire) yields no bindings, which fires — consistent with the fail-open
// stance above.
func (s *Scheduler) calendarBindings(ctx context.Context, source, ownerKind, ownerName, scheduleName string) (skip, only []string) {
	var skipRaw, onlyRaw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT skip_calendars, only_calendars
		  FROM definition_schedules
		 WHERE owner_source = ? AND owner_kind = ? AND owner_name = ? AND name = ?`,
		source, ownerKind, ownerName, scheduleName).Scan(&skipRaw, &onlyRaw)
	if err != nil {
		// sql.ErrNoRows is the ordinary case for an unregistered/renamed entry and
		// is not worth a log line at error level; anything else is.
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Error("scheduler: read calendar bindings", "owner", ownerName, "schedule", scheduleName, "err", err)
		}
		return nil, nil
	}
	return calendar.ParseNames(skipRaw.String), calendar.ParseNames(onlyRaw.String)
}
