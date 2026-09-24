package cronutil

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule specs beyond cron (Phase 2, the schedule-update plan §4).
//
// Cron is calendar-positional: it can say "every Wednesday at 17:00" but not
// "every 10 days", because a repeating interval needs a phase anchor and cron
// has none. The activation window's start (Phase 1) IS that anchor, so an
// interval schedule is expressible once the two are combined.
//
// A schedule entry is therefore one of three modes, resolved by ParseSpec:
//
//	cron      — a cron expression, optionally bounded by a window (Phase 1)
//	interval  — fire every N from the window start (the anchor is required)
//	once      — a window start with no cron and no interval: fire exactly once
//
// The modes are mutually exclusive and validated as such at every authoring
// boundary. All three return a cron.Schedule, so the scheduler registers them
// identically and the timing wheel stays unaware there is more than cron.

// ModeCron / ModeInterval / ModeOnce name a spec's resolved mode.
const (
	ModeCron     = "cron"
	ModeInterval = "interval"
	ModeOnce     = "once"
)

// MinInterval bounds how often an interval schedule may fire. A sub-minute
// interval is far more likely a typo ("30s") than an intent, and would spin the
// engine registering fires faster than a run can plausibly complete.
const MinInterval = time.Minute

var (
	// ErrSpecEmpty means no mode was specified at all.
	ErrSpecEmpty = errors.New("a schedule needs a cron expression, an interval, or a start date to run once")
	// ErrSpecConflict means more than one mode was specified.
	ErrSpecConflict = errors.New("cron and interval are mutually exclusive — set one, not both")
	// ErrIntervalNeedsAnchor means an interval was given with no phase anchor.
	ErrIntervalNeedsAnchor = errors.New("an interval schedule needs a start date to anchor it")
)

// dayShorthandRe matches the "Nd" day shorthand Go durations cannot express.
var dayShorthandRe = regexp.MustCompile(`^(\d+)\s*d$`)

// ParseInterval accepts a Go duration ("36h", "90m") or an "Nd" day shorthand
// ("7d", "10d"), which time.ParseDuration does not support and which is the
// form operators actually reach for. Returns 0 for an empty string.
func ParseInterval(s string) (time.Duration, error) {
	raw := strings.TrimSpace(strings.ToLower(s))
	if raw == "" {
		return 0, nil
	}
	if m := dayShorthandRe.FindStringSubmatch(raw); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid interval %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q (want a duration like 36h or 90m, or a day count like 7d)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid interval %q (must be positive)", s)
	}
	if d < MinInterval {
		return 0, fmt.Errorf("interval %q is shorter than the %s minimum", s, MinInterval)
	}
	return d, nil
}

// intervalSchedule fires every `every` from `anchor`, forever (bounded by the
// window's end, applied by the caller's decorator).
//
// The arithmetic is on ABSOLUTE instants — pure duration addition, no calendar
// walk — so an interval is immune to DST but also does not track wall-clock
// time-of-day across a DST boundary. That is the correct semantic for "every
// 36 hours"; an operator who means "every day at 5pm" wants cron, and the UI
// copy says so.
type intervalSchedule struct {
	anchor time.Time
	every  time.Duration
}

func (s intervalSchedule) Next(t time.Time) time.Time {
	if t.Before(s.anchor) {
		return s.anchor
	}
	// Strictly-after: a t landing exactly on a fire instant advances to the next.
	elapsed := t.Sub(s.anchor)
	n := elapsed/s.every + 1
	return s.anchor.Add(n * s.every)
}

// onceSchedule fires a single time, at `at`, and never again.
type onceSchedule struct{ at time.Time }

func (s onceSchedule) Next(t time.Time) time.Time {
	if t.Before(s.at) {
		return s.at
	}
	return time.Time{} // robfig: never again
}

// Spec is a schedule entry's firing rule in whichever mode it was authored.
type Spec struct {
	Cron     string
	Interval string
	Window   *Window
}

// Mode reports which of the three modes this spec is in, without validating it.
func (sp Spec) Mode() string {
	switch {
	case strings.TrimSpace(sp.Cron) != "":
		return ModeCron
	case strings.TrimSpace(sp.Interval) != "":
		return ModeInterval
	default:
		return ModeOnce
	}
}

// Validate checks mode exclusivity and each mode's own requirements, returning
// the same errors the API surfaces as 422s and the YAML layer as validation
// errors — one implementation, so Git-authored and in-app schedules are held to
// an identical contract.
func (sp Spec) Validate() error {
	hasCron := strings.TrimSpace(sp.Cron) != ""
	hasInterval := strings.TrimSpace(sp.Interval) != ""
	if hasCron && hasInterval {
		return ErrSpecConflict
	}
	switch {
	case hasCron:
		if !Valid(sp.Cron) {
			return fmt.Errorf("invalid cron expression %q", sp.Cron)
		}
	case hasInterval:
		if _, err := ParseInterval(sp.Interval); err != nil {
			return err
		}
		if sp.Window == nil || sp.Window.Start.IsZero() {
			return ErrIntervalNeedsAnchor
		}
	default:
		// Neither: a one-shot, which is only meaningful with an instant to fire at.
		if sp.Window == nil || sp.Window.Start.IsZero() {
			return ErrSpecEmpty
		}
	}
	return nil
}

// ParseSpec resolves a spec to a cron.Schedule with its window applied, so the
// scheduler registers all three modes through one seam.
func ParseSpec(sp Spec) (cron.Schedule, error) {
	if err := sp.Validate(); err != nil {
		return nil, err
	}
	switch sp.Mode() {
	case ModeCron:
		inner, err := Parse(sp.Cron)
		if err != nil {
			return nil, err
		}
		return withWindow(inner, sp.Window), nil
	case ModeInterval:
		every, err := ParseInterval(sp.Interval)
		if err != nil {
			return nil, err
		}
		// The anchor is the window start; the end bound still applies, so an
		// interval schedule can be given a stop date like any other.
		return withWindow(intervalSchedule{anchor: sp.Window.Start, every: every}, sp.Window), nil
	default:
		// A one-shot needs no window decorator: it fires once at the anchor and
		// returns the zero time thereafter. An end bound before that instant is
		// still honored, so route it through the decorator too.
		return withWindow(onceSchedule{at: sp.Window.Start}, sp.Window), nil
	}
}

// withWindow applies the activation window's end cutoff (and, for cron, its
// start deferral) to any inner schedule. The interval and once modes already
// begin at the anchor, so the start clamp is a no-op for them — it is applied
// uniformly rather than special-cased, since a redundant clamp cannot change a
// schedule that never fires before its anchor anyway.
func withWindow(inner cron.Schedule, win *Window) cron.Schedule {
	if win == nil {
		return inner
	}
	return specWindow{inner: inner, win: win}
}

type specWindow struct {
	inner cron.Schedule
	win   *Window
}

func (w specWindow) Next(t time.Time) time.Time {
	if !w.win.Start.IsZero() && t.Before(w.win.Start) {
		t = w.win.Start.Add(-time.Nanosecond)
	}
	next := w.inner.Next(t)
	if next.IsZero() || (!w.win.End.IsZero() && next.After(w.win.End)) {
		return time.Time{}
	}
	return next
}

// NextSpec returns the next fire time after `after` for any spec mode. It is
// the projection counterpart of ParseSpec, so "next run" in the UI is computed
// by exactly the rule the engine fires by.
func NextSpec(sp Spec, after time.Time, loc *time.Location) (time.Time, bool) {
	sched, err := ParseSpec(sp)
	if err != nil {
		return time.Time{}, false
	}
	next := sched.Next(after.In(locOr(loc)))
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// NextNSpec returns up to n fire times in (after, horizon] for any spec mode.
func NextNSpec(sp Spec, after, horizon time.Time, n int, loc *time.Location) []time.Time {
	sched, err := ParseSpec(sp)
	if err != nil || n <= 0 {
		return nil
	}
	out := make([]time.Time, 0, n)
	cur := after.In(locOr(loc))
	for len(out) < n {
		cur = sched.Next(cur)
		if cur.IsZero() || cur.After(horizon) {
			break
		}
		out = append(out, cur)
	}
	return out
}

// DescribeSpec renders a spec in one human phrase for logs and API summaries.
func DescribeSpec(sp Spec) string {
	switch sp.Mode() {
	case ModeCron:
		return "cron " + strings.TrimSpace(sp.Cron)
	case ModeInterval:
		return "every " + strings.TrimSpace(sp.Interval)
	default:
		return "once"
	}
}

// ParseDeadline validates and splits an 'HH:MM' wall-clock deadline (SL's
// must_finish_by).
//
// It lives here rather than in scheduler because BOTH authoring paths must
// reject the same strings — the in-app compose API and the Git YAML sync — and
// gitlab cannot import scheduler. Same argument as Spec.Validate, which is
// shared for exactly this reason.
func ParseDeadline(hhmm string) (hour, minute int, ok bool) {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// ValidDeadline reports whether an 'HH:MM' deadline parses.
func ValidDeadline(hhmm string) bool {
	_, _, ok := ParseDeadline(hhmm)
	return ok
}

// ── Concurrency policy vocabulary (QP) ───────────────────────────────────────
//
// Lives here for the same reason ParseDeadline does: BOTH authoring paths must
// agree on what a policy is — the Git YAML sync and the in-app compose API —
// and gitlab cannot import api or scheduler. Before this there were two
// independent switch statements, and both SILENTLY COERCED an unrecognised
// value to Allow, which is how a `Queue` typed into YAML would have become
// "overlap freely" with no error anywhere.
const (
	// PolicyAllow lets runs overlap.
	PolicyAllow = "Allow"
	// PolicyForbid skips a fire that meets an active run, recording the skip.
	PolicyForbid = "Forbid"
	// PolicyQueue parks the fire until the gate clears, then promotes it.
	PolicyQueue = "Queue"
)

// ConcurrencyPolicies is the closed set, in the order a UI should offer them
// (increasing strictness). It is what the OpenAPI enum is checked against.
var ConcurrencyPolicies = []string{PolicyAllow, PolicyForbid, PolicyQueue}

// ValidPolicy reports whether p is a known policy.
func ValidPolicy(p string) bool {
	return slices.Contains(ConcurrencyPolicies, p)
}

// NormalizePolicy resolves a stored or authored value to a known policy,
// degrading to Allow.
//
// The degradation is deliberate and matches the JR-Q5 precedent for
// prompt_enforcement: a typo in Git must never take a job offline or make it
// stricter than its author asked. Callers that can report an error to a human
// (the compose API) should use ValidPolicy and 422 instead.
func NormalizePolicy(p string) string {
	if ValidPolicy(p) {
		return p
	}
	return PolicyAllow
}

// HoldsGate reports whether a policy makes a run hold its concurrency key
// against the next fire. Allow does not; Forbid and Queue both do — they differ
// only in what happens to the fire that meets it (skipped vs parked), which is
// why every gate check asks THIS rather than comparing to Forbid.
func HoldsGate(p string) bool { return p == PolicyForbid || p == PolicyQueue }

// ConcurrencyKey returns the gate key a run must carry, and is the ONE place
// that rule lives (R2-3, the rbac2 plan).
//
// Six producers build this key — cron fire, manual trigger, reactor, workflow
// step, skipped workflow step, and file-watch arrival — and RX-25 is the
// standing lesson about what happens when they disagree: a run that composes
// the key differently neither queues behind nor collides with the others, so a
// job whose whole point is never to overlap silently runs twice. They agree by
// calling this.
//
// Precedence:
//   - custom: jobs.concurrency_key, an operator-authored value. Deliberately
//     free text and deliberately NOT re-keyed by AF-4b — it is a shared
//     namespace, the way two different jobs are made to hold one gate.
//   - uid: the job's permanent identity. The default since R2-3, because the
//     name it replaced will one day belong to more than one job, and two
//     agencies' jobs sharing a gate by accident is a deadlock nobody asked for.
//   - source/name: the pre-R2-3 composition, kept as a fallback for a job whose
//     uid is somehow unset. Never silently produces an EMPTY key, which would
//     drop the run out of the gate entirely (the partial unique index ignores
//     NULL/” keys) and turn a Forbid job into an Allow one.
func ConcurrencyKey(custom, uid, source, name string) string {
	if custom != "" {
		return custom
	}
	if uid != "" {
		return uid
	}
	return source + "/" + name
}
