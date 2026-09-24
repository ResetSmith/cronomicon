// Package cronutil centralizes cron expression parsing and next-run projection.
//
// It is a leaf package (imported by scheduler, gitlab validation, and the API
// projection layer) so all three agree on exactly which expressions are valid
// and when they fire. Parsing mirrors the scheduler's historical behavior:
//   - 6-field expressions (with seconds, GitLab-CI style) are tried first.
//   - 5-field standard POSIX expressions fall back by prepending "0 " (seconds).
//
// Evaluation takes an explicit *time.Location — the same effective app zone the
// running cron engine uses (settings.ResolveEffectiveTimezone) — so projected
// next-run times match actual fires. A nil location degrades to time.Local.
package cronutil

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// parser6 accepts 6-field expressions (seconds minute hour dom month dow).
var parser6 = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// parser5 accepts standard 5-field expressions (minute hour dom month dow).
var parser5 = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Parse returns a cron.Schedule for expr, trying 6-field then 5-field, matching
// the scheduler's registration behavior. An empty or "Manual" expression is an
// error — callers should filter those before parsing.
//
// The "@every <duration>" descriptor is rejected even though robfig accepts it:
// it is an unanchored interval, which this system expresses with an anchored
// schedule window rather than a cron expression, and no UI surface can render
// or validate it. Calendar descriptors (@daily, @weekly, …) remain accepted —
// they are deterministic calendar expressions with exact 5-field equivalents.
func Parse(expr string) (cron.Schedule, error) {
	if isEveryDescriptor(expr) {
		return nil, fmt.Errorf("invalid cron expression %q: @every intervals are not supported (use a cron expression)", expr)
	}
	if sched, err := parser6.Parse(expr); err == nil {
		return sched, nil
	}
	sched, err := parser5.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return sched, nil
}

// isEveryDescriptor reports whether expr is robfig's "@every <duration>" form.
func isEveryDescriptor(expr string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(expr)), "@every")
}

// Valid reports whether expr parses as either a 6- or 5-field expression.
func Valid(expr string) bool {
	_, err := Parse(expr)
	return err == nil
}

// locOr returns loc, or time.Local when loc is nil — so callers that haven't
// resolved a zone yet still evaluate in a sane default rather than panicking.
func locOr(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}

// Next returns the next fire time strictly after `after` (evaluated in loc), and
// ok=false if the expression is invalid or has no future occurrence.
func Next(expr string, after time.Time, loc *time.Location) (time.Time, bool) {
	sched, err := Parse(expr)
	if err != nil {
		return time.Time{}, false
	}
	next := sched.Next(after.In(locOr(loc)))
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// NextN returns up to n fire times strictly after `after` and at or before
// `horizon` (evaluated in loc), soonest-first. It stops early at the horizon or
// after n occurrences, whichever comes first — so it is safe to call with a
// large n and a bounded horizon.
func NextN(expr string, after, horizon time.Time, n int, loc *time.Location) []time.Time {
	return NextNWindowed(expr, after, horizon, n, loc, nil)
}

// Window is a schedule entry's activation window: fires before Start or after
// End are suppressed. A nil *Window, or a zero bound within it, is unbounded on
// that side. Bounds are absolute instants — unlike the cron fields themselves,
// they do not depend on the evaluation location.
type Window struct {
	Start time.Time
	End   time.Time
}

// Active reports whether now falls inside the window.
func (w *Window) Active(now time.Time) bool {
	if w == nil {
		return true
	}
	if !w.Start.IsZero() && now.Before(w.Start) {
		return false
	}
	if !w.End.IsZero() && now.After(w.End) {
		return false
	}
	return true
}

// Pending reports whether the window has a start bound that is still in the future.
func (w *Window) Pending(now time.Time) bool {
	return w != nil && !w.Start.IsZero() && now.Before(w.Start)
}

// Expired reports whether the window has an end bound that has already passed.
func (w *Window) Expired(now time.Time) bool {
	return w != nil && !w.End.IsZero() && now.After(w.End)
}

// clampAfter advances `after` to the window's start when the start is later, so
// the underlying schedule never yields a fire time before the window opens.
func (w *Window) clampAfter(after time.Time) time.Time {
	if w == nil || w.Start.IsZero() || !after.Before(w.Start) {
		return after
	}
	// Step back a nanosecond so a fire exactly at Start is not skipped: the
	// cron Next contract is strictly-after.
	return w.Start.Add(-time.Nanosecond)
}

// past reports whether t lies beyond the window's end bound.
func (w *Window) past(t time.Time) bool {
	return w != nil && !w.End.IsZero() && t.After(w.End)
}

// NextNWindowed is NextN, restricted to an activation window.
func NextNWindowed(expr string, after, horizon time.Time, n int, loc *time.Location, win *Window) []time.Time {
	sched, err := Parse(expr)
	if err != nil || n <= 0 {
		return nil
	}
	out := make([]time.Time, 0, n)
	cur := win.clampAfter(after).In(locOr(loc))
	for len(out) < n {
		cur = sched.Next(cur)
		if cur.IsZero() || cur.After(horizon) || win.past(cur) {
			break
		}
		out = append(out, cur)
	}
	return out
}

// NewWindow builds a *Window from optional bounds, returning nil when both are
// absent so unbounded entries stay on the zero-overhead path.
func NewWindow(start, end *time.Time) *Window {
	if start == nil && end == nil {
		return nil
	}
	w := &Window{}
	if start != nil {
		w.Start = *start
	}
	if end != nil {
		w.End = *end
	}
	return w
}

// UntilWallClock returns how long until the next occurrence of the UTC
// wall-clock time hhmm ("15:04"). An unparseable or empty hhmm falls back to
// fallback (which must parse). Anchoring to the wall clock rather than to
// "24h from boot" means a process that restarts more often than daily cannot
// keep pushing a daily task past 24h (PP-H6) — shared by the retention/backup
// sweep and the log-archive sync's daily mode (SL-2).
func UntilWallClock(hhmm, fallback string, now time.Time) time.Duration {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		t, err = time.Parse("15:04", fallback)
		if err != nil {
			t = time.Date(0, 1, 1, 2, 0, 0, 0, time.UTC)
		}
	}
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}
