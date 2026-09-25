// Package calendar resolves working-calendar suppression for schedule entries
// (the calendar-update plan, CAL-3).
//
// A calendar is a named set of wall-clock dates. A schedule entry may name
// calendars in two roles — skip (a fire landing on one of these days is
// suppressed) and only (a fire is suppressed unless it lands on one of these
// days) — and a calendar marked global has its days unioned into every entry's
// skip set whether or not the entry names it.
//
// # Why this is a leaf package
//
// Suppression is decided in TWO places that must never disagree: the scheduler,
// which suppresses the fire, and /schedules/upcoming, which predicts it. If the
// engine suppresses a fire the projection still promises, the product lies
// twice — it promises a run that will not happen and it hides the holiday gap
// the operator specifically wants to see. So the rule lives here as a pure
// function with two callers, rather than as two implementations of a day rule.
// This package therefore imports nothing from scheduler, api, or gitlab.
//
// # Why not a cron decorator
//
// Wrapping cron.Schedule to advance Next() past excluded days would make the
// engine and every projection agree by construction, but a fire that never
// happens leaves NOTHING BEHIND — and "prove this job did not run on the
// holiday, and that it was deliberate" is the question this feature exists to
// answer. See §2.3 for the other two reasons (calendar edits would force a
// scheduler reload; Next() on an exhausted only-calendar would scan forward
// forever).
package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DayFormat is the strict wall-clock day layout stored in calendar_days.day.
const DayFormat = "2006-01-02"

// DaySet maps a 'YYYY-MM-DD' day to its label ("Independence Day (observed)").
// The label may be empty; presence in the map is what suppresses.
type DaySet map[string]string

// Calendar is one loaded calendar: its days plus the two behavioural flags that
// govern how its suppressions apply and whether they are recorded.
type Calendar struct {
	Name string
	// Global unions this calendar's days into EVERY entry's skip set (§2.8).
	Global bool
	// RecordSuppressed governs only-mode suppressions ONLY (CAL-Q4): skip-mode
	// suppressions always record, because those are the compliance question.
	RecordSuppressed bool
	Days             DaySet
}

// Set is the loaded calendars by name. Names are unambiguous by construction:
// calendars are cronomicon-source only (CAL-Q2/CAL-Q7), so there is no shadowing
// rule to decide.
type Set map[string]Calendar

// Verdict is the outcome of evaluating one fire instant.
type Verdict struct {
	// Suppressed reports whether the fire must not happen.
	Suppressed bool
	// Calendar names the calendar responsible, for the audit row's
	// suppressed_by_calendar column. Empty when not suppressed.
	Calendar string
	// Label is the day's label in that calendar, when suppression was caused by
	// a day being PRESENT (skip polarity). Empty for only-polarity suppression,
	// where the cause is a day's absence and there is no label to name.
	Label string
	// Polarity is "skip" or "only" — which rule fired.
	Polarity string
	// Record reports whether this suppression should be written to History.
	// Always true for skip; for only, true only when a named calendar sets
	// RecordSuppressed (CAL-Q4).
	Record bool
}

// DayOf renders an instant as the wall-clock day used for matching.
//
// ALWAYS pass the effective app zone — the same zone the cron engine fires in —
// never time.UTC. A holiday is a wall-clock day: matching in UTC would suppress
// the wrong side of midnight for every deployment east or west of Greenwich.
// A nil loc degrades to time.Local rather than silently becoming UTC, because
// UTC is the one answer that is wrong on purpose here.
func DayOf(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format(DayFormat)
}

// ValidDay reports whether s is a strict 'YYYY-MM-DD' real calendar date.
// Used by the authoring boundaries (CAL-4/CAL-5) and by the day-table writer;
// "2026-02-30" parses as a date in some layouts but is not a real day.
func ValidDay(s string) bool {
	t, err := time.Parse(DayFormat, s)
	return err == nil && t.Format(DayFormat) == s
}

// Evaluate applies §2.2's rule to one instant's day.
//
//  1. only is non-empty and day is NOT in the union of those calendars → suppressed
//  2. day is in any skip calendar, or in any GLOBAL calendar → suppressed
//  3. otherwise → fire
//
// Skip is a veto and is evaluated last, so naming both roles is legal and
// unambiguous ("only on fiscal-close days, but never on a holiday").
//
// # Dangling and empty calendars
//
// A named calendar absent from sets, or present with zero days, contributes the
// empty set. No special case is needed: each polarity then fails toward its own
// authored intent — a dangling skip binding FIRES (a visible, correctable
// policy violation) while a dangling only binding NEVER FIRES (silence, which
// is what "only run on these days" asked for). Fail-open-for-skip and
// fail-closed-for-only fall out of set arithmetic rather than being legislated.
// Dangling names are nonetheless prevented at both authoring boundaries and
// flagged by CAL-16; this is the backstop, not the plan.
// # Evaluation order, and why it is not §2.2's numbered order
//
// §2.2 numbers the only-check first and the skip-check second, noting that
// "skip is a veto and is evaluated last". Both orders produce the SAME
// SUPPRESSION DECISION in all four cases, because a day failing either rule is
// suppressed either way. They differ only in ATTRIBUTION — which calendar the
// audit row names — and there the numbered order is actively wrong:
//
//	only:[fiscal-close] + skip:[federal-holidays], firing on a holiday that is
//	not a fiscal-close day
//
// Returning the only-verdict first makes this a Record:false only-suppression,
// so the holiday skip leaves NO audit row — contradicting CAL-Q4's decided
// invariant that skip suppressions always record. The same shape hides a global
// change freeze (§2.8 explicitly wants the freeze named, so "nobody spends an
// afternoon wondering why a job stopped firing"), narrowed to only-bound
// entries. So the veto is evaluated first, which is also the plainer reading of
// "skip is a veto": skip wins whenever it applies.
func Evaluate(sets Set, skip, only []string, day string) Verdict {
	// (1) skip-polarity, the veto: bound calendars first, then the global tier.
	// Bound names are checked in the operator's authored order so the reported
	// reason is stable across evaluations.
	for _, name := range skip {
		if label, ok := sets[name].Days[day]; ok {
			return Verdict{Suppressed: true, Calendar: name, Label: label, Polarity: "skip", Record: true}
		}
	}
	// Global calendars apply to every entry, including entries with no bindings
	// at all (§2.8) — which is exactly what makes a change freeze one checkbox
	// instead of N schedule edits. Iterated in sorted order because Go map order
	// is randomised and an unstable suppression reason would be a bad audit row.
	for _, name := range sortedNames(sets) {
		c := sets[name]
		if !c.Global {
			continue
		}
		if label, ok := c.Days[day]; ok {
			return Verdict{Suppressed: true, Calendar: name, Label: label, Polarity: "skip", Record: true}
		}
	}

	// (2) only-polarity: the day must appear in the union of the named calendars.
	if len(only) > 0 {
		for _, name := range only {
			if _, ok := sets[name].Days[day]; ok {
				return Verdict{} // a run day, and no veto applied above
			}
		}
		// Union, not intersection: an operator naming two calendars means "either
		// of these" — an intersection is expressible by making one calendar.
		// Record only if some named calendar opted in (CAL-Q4).
		record := false
		for _, name := range only {
			if sets[name].RecordSuppressed {
				record = true
				break
			}
		}
		return Verdict{
			Suppressed: true,
			// The audit column takes the FIRST named calendar so the stored value
			// is a single deterministic name the History filter can match. With
			// several only-calendars the others are not named anywhere — a narrow
			// corner, since one calendar is the overwhelmingly common case and
			// only-mode recording is off by default.
			Calendar: only[0],
			Polarity: "only",
			Record:   record,
		}
	}

	return Verdict{}
}

// Reason renders the human-readable queued_reason for a suppression. The
// structured provenance lives in runs.suppressed_by_calendar, NOT here — this
// text is free to be copy-edited without breaking the audit query.
func Reason(v Verdict) string {
	if !v.Suppressed {
		return ""
	}
	if v.Polarity == "only" {
		return fmt.Sprintf("Skipped: not a run day in calendar %q", v.Calendar)
	}
	if v.Label != "" {
		return fmt.Sprintf("Skipped: suppressed by calendar %q (%s)", v.Calendar, v.Label)
	}
	return fmt.Sprintf("Skipped: suppressed by calendar %q", v.Calendar)
}

// Load resolves calendar names to their day sets in ONE query, plus every
// global calendar whether or not it was named.
//
// The global union is why there is no empty-bindings fast path: an entry naming
// no calendars can still be suppressed by a change freeze, so every fire pays
// this read. Callers with many entries (the /schedules/upcoming projection)
// must call Load ONCE for every name across every entry rather than per entry,
// preserving the CC.9 fixed-query-count discipline.
func Load(ctx context.Context, database *sql.DB, names []string) (Set, error) {
	out := Set{}

	// Deduplicate and drop blanks so the IN list is minimal and stable.
	seen := map[string]bool{}
	var want []string
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		want = append(want, n)
	}

	// The global clause means this query is never pointless, even with no names.
	q := `SELECT c.name, c.global, c.record_suppressed, d.day, d.label
	        FROM calendars c
	        LEFT JOIN calendar_days d
	               ON d.calendar_source = c.source AND d.calendar_name = c.name
	       WHERE c.global = 1`
	args := []any{}
	if len(want) > 0 {
		q += " OR c.name IN (" + strings.TrimSuffix(strings.Repeat("?,", len(want)), ",") + ")"
		for _, n := range want {
			args = append(args, n)
		}
	}

	rows, err := database.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load calendars: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name       string
			global     int
			recordSupp int
			day, label sql.NullString
		)
		if err := rows.Scan(&name, &global, &recordSupp, &day, &label); err != nil {
			return nil, fmt.Errorf("scan calendar day: %w", err)
		}
		c, ok := out[name]
		if !ok {
			c = Calendar{Name: name, Global: global == 1, RecordSuppressed: recordSupp == 1, Days: DaySet{}}
		}
		// A calendar with zero days still lands in the set (LEFT JOIN, NULL day)
		// so callers can tell "exists but empty" from "does not exist" — CAL-16
		// needs that distinction even though Evaluate treats both as empty.
		if day.Valid {
			c.Days[day.String] = label.String
		}
		out[name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate calendar days: %w", err)
	}
	return out, nil
}

// LoadFlags returns the set of existing calendar names and the subset that are
// global — everything ValidateBinding needs, in one query. Separate from Load
// because validation cares about existence and flags, never about days.
func LoadFlags(ctx context.Context, database *sql.DB) (known, global map[string]bool, err error) {
	rows, err := database.QueryContext(ctx, `SELECT name, global FROM calendars`)
	if err != nil {
		return nil, nil, fmt.Errorf("load calendar flags: %w", err)
	}
	defer rows.Close()
	known, global = map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var name string
		var g int
		if err := rows.Scan(&name, &g); err != nil {
			return nil, nil, fmt.Errorf("scan calendar flag: %w", err)
		}
		known[name] = true
		if g == 1 {
			global[name] = true
		}
	}
	return known, global, rows.Err()
}

// ValidateBinding is the ONE contract every authoring boundary enforces for a
// schedule entry's calendar bindings (CAL-5, CAL-9, CAL-27). It is pure so the
// in-app compose path and the Git sync path share it exactly — the same
// discipline ParseSpec already keeps for schedule modes — and returns the
// normalised name lists alongside any refusal.
//
// Three refusals, each for a distinct failure it prevents:
//
//   - Unknown calendar name. §2.2 defines dangling names by set arithmetic as a
//     BACKSTOP, not a plan: a dangling skip binding silently stops suppressing
//     (a policy violation nobody is told about) and a dangling only binding
//     silently never fires. Authoring time is the only cheap moment to catch the
//     typo.
//   - A name in BOTH roles on one entry. Not merely redundant — incoherent: the
//     calendar would be at once the entry's run-day list and its veto, so every
//     listed day would be both required and forbidden.
//   - A GLOBAL calendar in the `only` role (CAL-27). A global calendar is unioned
//     into every entry's SKIP set, so using one as a run-day list yields an entry
//     that may only run on days it is also globally forbidden from running on —
//     an entry that can never fire, reached by two individually sensible clicks.
func ValidateBinding(skip, only []string, known, global map[string]bool) (outSkip, outOnly []string, verr string) {
	outSkip = ParseNamesSlice(skip)
	outOnly = ParseNamesSlice(only)
	if len(outSkip) == 0 && len(outOnly) == 0 {
		return nil, nil, ""
	}

	inSkip := map[string]bool{}
	for _, n := range outSkip {
		inSkip[n] = true
	}
	for _, n := range outOnly {
		if inSkip[n] {
			return nil, nil, "calendar " + n + " is named in both skipCalendars and onlyCalendars; " +
				"a calendar cannot be an entry's run-day list and its veto at once"
		}
	}

	var missing []string
	for _, n := range append(append([]string{}, outSkip...), outOnly...) {
		if !known[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, nil, "unknown calendar(s): " + strings.Join(missing, ", ")
	}
	for _, n := range outOnly {
		if global[n] {
			return nil, nil, "calendar " + n + " is global and cannot be used as a run-day (only) calendar; " +
				"a global calendar is unioned into every entry's SKIP set"
		}
	}
	return outSkip, outOnly, ""
}

// ParseNames decodes a skip_calendars / only_calendars JSON column.
//
// Tolerant by design: NULL, empty, "null", or malformed JSON all yield no
// names. A binding column that cannot be parsed must not wedge the fire path —
// it degrades to "no binding", which for skip polarity means the job runs (a
// visible outcome) rather than silently never running.
func ParseNames(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MarshalNames encodes names for storage, returning "" for an empty set so the
// caller stores SQL NULL.
//
// The empty case MUST stay "" rather than "[]": scheduleContentHash appends its
// segments only when non-empty (the AW-Q4 conditional-hash pattern), so an
// unbound row must serialise to exactly what it serialised to before this
// feature existed or every pre-feature row's stored digest changes and the next
// sync reports the whole catalogue as modified.
func MarshalNames(names []string) string {
	clean := ParseNamesSlice(names)
	if len(clean) == 0 {
		return ""
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseNamesSlice normalises an in-memory name list the same way ParseNames
// normalises a stored one: trimmed, de-duplicated, order preserved.
func ParseNamesSlice(names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func sortedNames(sets Set) []string {
	out := make([]string, 0, len(sets))
	for n := range sets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
