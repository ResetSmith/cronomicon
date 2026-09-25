package calendar

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// CAL-20 — the resolver's rule table. These tests are the specification of
// §2.2: both polarities, both empty-set cases, skip beating only, union across
// only-calendars, the global tier, and the app-zone day boundary.

func set(cals ...Calendar) Set {
	out := Set{}
	for _, c := range cals {
		out[c.Name] = c
	}
	return out
}

func cal(name string, days ...string) Calendar {
	c := Calendar{Name: name, Days: DaySet{}}
	for _, d := range days {
		c.Days[d] = ""
	}
	return c
}

func TestEvaluateSkipPolarity(t *testing.T) {
	sets := set(Calendar{Name: "holidays", Days: DaySet{"2026-07-03": "Independence Day (observed)"}})

	v := Evaluate(sets, []string{"holidays"}, nil, "2026-07-03")
	if !v.Suppressed {
		t.Fatal("a fire landing on a skip-calendar day must be suppressed")
	}
	if v.Calendar != "holidays" || v.Polarity != "skip" {
		t.Errorf("verdict = %+v, want calendar=holidays polarity=skip", v)
	}
	if v.Label != "Independence Day (observed)" {
		t.Errorf("label = %q, want the day's label — History shows this as the reason", v.Label)
	}
	if !v.Record {
		t.Error("skip-polarity suppressions must ALWAYS record (CAL-Q4): this is the compliance question")
	}

	if v := Evaluate(sets, []string{"holidays"}, nil, "2026-07-06"); v.Suppressed {
		t.Errorf("an ordinary day must fire, got %+v", v)
	}
}

func TestEvaluateOnlyPolarity(t *testing.T) {
	sets := set(cal("fiscal-close", "2026-09-30"))

	if v := Evaluate(sets, nil, []string{"fiscal-close"}, "2026-09-30"); v.Suppressed {
		t.Errorf("a day inside the only-set must fire, got %+v", v)
	}

	v := Evaluate(sets, nil, []string{"fiscal-close"}, "2026-09-29")
	if !v.Suppressed || v.Polarity != "only" {
		t.Fatalf("a day outside the only-set must be suppressed, got %+v", v)
	}
	if v.Record {
		t.Error("only-mode suppression must NOT record by default (CAL-Q4) — a weekdays-only entry would write ten rows every weekend")
	}
}

func TestEvaluateOnlyRecordsWhenCalendarOptsIn(t *testing.T) {
	c := cal("fiscal-close", "2026-09-30")
	c.RecordSuppressed = true
	v := Evaluate(set(c), nil, []string{"fiscal-close"}, "2026-09-29")
	if !v.Suppressed || !v.Record {
		t.Fatalf("verdict = %+v, want suppressed and recorded once the calendar opts in", v)
	}
}

// Skip is a veto evaluated last, so naming both roles is legal: "only on
// fiscal-close days, but never on a holiday".
func TestEvaluateSkipBeatsOnly(t *testing.T) {
	sets := set(
		cal("fiscal-close", "2026-09-30"),
		Calendar{Name: "holidays", Days: DaySet{"2026-09-30": "Closure"}},
	)
	v := Evaluate(sets, []string{"holidays"}, []string{"fiscal-close"}, "2026-09-30")
	if !v.Suppressed {
		t.Fatal("a day allowed by only but present in skip must be suppressed")
	}
	if v.Polarity != "skip" || v.Calendar != "holidays" {
		t.Errorf("verdict = %+v, want the SKIP calendar named — that is the operative rule", v)
	}
}

// The case both reviewers caught: an entry carrying BOTH roles, suppressed on a
// day that is off the only-set AND inside a skip calendar. Evaluating only-first
// would report a Record:false only-suppression and leave no audit row at all —
// silently contradicting CAL-Q4's "skip suppressions always record" on exactly
// the days the compliance question is about.
func TestEvaluateSkipWinsWhenDayIsAlsoOffTheOnlySet(t *testing.T) {
	sets := set(
		cal("fiscal-close", "2026-09-30"),
		Calendar{Name: "holidays", Days: DaySet{"2026-07-03": "Independence Day (observed)"}},
	)
	v := Evaluate(sets, []string{"holidays"}, []string{"fiscal-close"}, "2026-07-03")
	if !v.Suppressed {
		t.Fatal("must be suppressed")
	}
	if v.Polarity != "skip" || v.Calendar != "holidays" {
		t.Errorf("verdict = %+v, want attribution to the SKIP calendar", v)
	}
	if !v.Record {
		t.Error("a skip suppression must record even when an only binding is also present (CAL-Q4)")
	}
	if v.Label != "Independence Day (observed)" {
		t.Errorf("label = %q, want the holiday's label", v.Label)
	}
}

// Same shape for the global tier: a change freeze must be NAMED even on an entry
// whose only-binding would independently have suppressed the day (§2.8).
func TestEvaluateGlobalNamedEvenWhenOnlySetAlsoMisses(t *testing.T) {
	sets := set(
		cal("fiscal-close", "2026-09-30"),
		Calendar{Name: "change-freeze-2026q3", Global: true, Days: DaySet{"2026-08-15": "Q3 freeze"}},
	)
	v := Evaluate(sets, nil, []string{"fiscal-close"}, "2026-08-15")
	if !v.Suppressed || v.Calendar != "change-freeze-2026q3" || !v.Record {
		t.Errorf("verdict = %+v, want the freeze named and recorded", v)
	}
}

// A day that is a run day and carries no veto still fires — the reorder must not
// have turned the only-check into a no-op.
func TestEvaluateRunDayWithSkipBindingStillFires(t *testing.T) {
	sets := set(cal("fiscal-close", "2026-09-30"), cal("holidays", "2026-07-03"))
	if v := Evaluate(sets, []string{"holidays"}, []string{"fiscal-close"}, "2026-09-30"); v.Suppressed {
		t.Errorf("a fiscal-close day that is not a holiday must fire, got %+v", v)
	}
}

// The operator reaching for two only-calendars means "either of these"; an
// intersection is expressible by making one calendar.
func TestEvaluateOnlyIsUnionNotIntersection(t *testing.T) {
	sets := set(cal("a", "2026-01-05"), cal("b", "2026-01-06"))
	for _, day := range []string{"2026-01-05", "2026-01-06"} {
		if v := Evaluate(sets, nil, []string{"a", "b"}, day); v.Suppressed {
			t.Errorf("day %s is in the union of the only-calendars and must fire, got %+v", day, v)
		}
	}
	if v := Evaluate(sets, nil, []string{"a", "b"}, "2026-01-07"); !v.Suppressed {
		t.Error("a day in neither only-calendar must be suppressed")
	}
}

// §2.2's dangling-reference answer, which falls out of set arithmetic rather
// than being legislated: each polarity fails toward its own authored intent.
func TestEvaluateEmptyAndDanglingSets(t *testing.T) {
	empty := set(cal("empty"))

	// skip → nothing to skip → FIRES. A visible, correctable policy violation.
	if v := Evaluate(Set{}, []string{"missing"}, nil, "2026-07-03"); v.Suppressed {
		t.Errorf("a dangling skip binding must fire (fail open), got %+v", v)
	}
	if v := Evaluate(empty, []string{"empty"}, nil, "2026-07-03"); v.Suppressed {
		t.Errorf("an empty skip calendar must fire, got %+v", v)
	}

	// only → no allowed days → NEVER FIRES. Silence is what "only run on these
	// days" asked for.
	if v := Evaluate(Set{}, nil, []string{"missing"}, "2026-07-03"); !v.Suppressed {
		t.Error("a dangling only binding must suppress (fail closed)")
	}
	if v := Evaluate(empty, nil, []string{"empty"}, "2026-07-03"); !v.Suppressed {
		t.Error("an empty only calendar must suppress")
	}

	// No bindings at all → always fires.
	if v := Evaluate(Set{}, nil, nil, "2026-07-03"); v.Suppressed {
		t.Errorf("an entry with no bindings must fire, got %+v", v)
	}
}

// §2.8 — a global calendar suppresses an entry that names NO calendars, which is
// the whole point: a change freeze must not require editing N schedules.
func TestEvaluateGlobalTier(t *testing.T) {
	g := Calendar{Name: "change-freeze-2026q3", Global: true, Days: DaySet{"2026-08-15": "Q3 freeze"}}
	v := Evaluate(set(g), nil, nil, "2026-08-15")
	if !v.Suppressed {
		t.Fatal("a global calendar must suppress an entry with no bindings of its own")
	}
	if v.Calendar != "change-freeze-2026q3" {
		t.Errorf("calendar = %q, want the global calendar NAMED — otherwise nobody can tell why a job stopped firing", v.Calendar)
	}
	if !v.Record {
		t.Error("global suppression is skip-polarity and must record")
	}
	if v := Evaluate(set(g), nil, nil, "2026-08-16"); v.Suppressed {
		t.Error("a day outside the global calendar must fire")
	}
}

// A global calendar is unioned into the SKIP set, so it vetoes even a day the
// only-binding allows.
func TestEvaluateGlobalBeatsOnlyAllowedDay(t *testing.T) {
	sets := set(
		cal("fiscal-close", "2026-08-15"),
		Calendar{Name: "freeze", Global: true, Days: DaySet{"2026-08-15": "freeze"}},
	)
	v := Evaluate(sets, nil, []string{"fiscal-close"}, "2026-08-15")
	if !v.Suppressed || v.Calendar != "freeze" {
		t.Errorf("verdict = %+v, want suppression by the global freeze", v)
	}
}

// CAL-7 — the day is a WALL-CLOCK day in the app zone. A 23:30 local fire on a
// holiday is suppressed; the same instant read as UTC would not be.
func TestDayOfUsesAppZoneNotUTC(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2026-07-03 23:30 in New York is 2026-07-04 03:30 UTC.
	instant := time.Date(2026, 7, 3, 23, 30, 0, 0, ny)

	if got := DayOf(instant, ny); got != "2026-07-03" {
		t.Fatalf("DayOf(app zone) = %q, want 2026-07-03", got)
	}
	if got := DayOf(instant, time.UTC); got != "2026-07-04" {
		t.Fatalf("DayOf(UTC) = %q, want 2026-07-04 — this is the wrong answer the feature must avoid", got)
	}

	holidays := set(Calendar{Name: "holidays", Days: DaySet{"2026-07-03": "Independence Day (observed)"}})
	if v := Evaluate(holidays, []string{"holidays"}, nil, DayOf(instant, ny)); !v.Suppressed {
		t.Error("a 23:30 local fire on the holiday must be suppressed")
	}
	if v := Evaluate(holidays, []string{"holidays"}, nil, DayOf(instant, time.UTC)); v.Suppressed {
		t.Error("the UTC reading must NOT match — proving the zone is what decides the day")
	}
}

func TestValidDay(t *testing.T) {
	for _, ok := range []string{"2026-01-01", "2026-12-31", "2024-02-29"} {
		if !ValidDay(ok) {
			t.Errorf("ValidDay(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "2026-1-1", "2026-02-30", "2026-13-01", "07/03/2026", "2026-07-03T00:00:00Z"} {
		if ValidDay(bad) {
			t.Errorf("ValidDay(%q) = true, want false", bad)
		}
	}
}

// MarshalNames must return "" (⇒ SQL NULL) for an empty set: scheduleContentHash
// appends its segments only when non-empty, so an unbound row has to serialise
// exactly as it did before this feature or every stored digest changes.
func TestNameEncodingRoundTrip(t *testing.T) {
	if got := MarshalNames(nil); got != "" {
		t.Errorf("MarshalNames(nil) = %q, want \"\" so the column stays NULL", got)
	}
	if got := MarshalNames([]string{"", "  "}); got != "" {
		t.Errorf("MarshalNames(blanks) = %q, want \"\"", got)
	}

	raw := MarshalNames([]string{"holidays", "holidays", " freeze "})
	if raw != `["holidays","freeze"]` {
		t.Errorf("MarshalNames = %q, want de-duplicated and trimmed in authored order", raw)
	}
	got := ParseNames(raw)
	if len(got) != 2 || got[0] != "holidays" || got[1] != "freeze" {
		t.Errorf("ParseNames(%q) = %v, want [holidays freeze]", raw, got)
	}

	// Tolerant decode: a binding column that cannot be parsed degrades to "no
	// binding" rather than wedging the fire path.
	for _, bad := range []string{"", "null", "   ", "not json", `{"a":1}`, `[]`} {
		if got := ParseNames(bad); got != nil {
			t.Errorf("ParseNames(%q) = %v, want nil", bad, got)
		}
	}
}

func TestReasonText(t *testing.T) {
	withLabel := Reason(Verdict{Suppressed: true, Calendar: "holidays", Label: "Independence Day (observed)", Polarity: "skip"})
	if withLabel != `Skipped: suppressed by calendar "holidays" (Independence Day (observed))` {
		t.Errorf("reason = %q", withLabel)
	}
	if got := Reason(Verdict{Suppressed: true, Calendar: "holidays", Polarity: "skip"}); got != `Skipped: suppressed by calendar "holidays"` {
		t.Errorf("unlabelled reason = %q", got)
	}
	if got := Reason(Verdict{Suppressed: true, Calendar: "fiscal-close", Polarity: "only"}); got != `Skipped: not a run day in calendar "fiscal-close"` {
		t.Errorf("only-mode reason = %q", got)
	}
	if got := Reason(Verdict{}); got != "" {
		t.Errorf("unsuppressed reason = %q, want empty", got)
	}
}

// ── Load ───────────────────────────────────────────────────────────────────

func mustPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "cal.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func seedCalendar(t *testing.T, pool *sql.DB, name string, global, record bool, days map[string]string) {
	t.Helper()
	ctx := context.Background()
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO calendars (source, name, global, record_suppressed, created_at) VALUES ('cronomicon', ?, ?, ?, '2026-08-06T00:00:00Z')`,
		name, b(global), b(record)); err != nil {
		t.Fatalf("seed calendar %s: %v", name, err)
	}
	for day, label := range days {
		if _, err := pool.ExecContext(ctx,
			`INSERT INTO calendar_days (calendar_source, calendar_name, day, label) VALUES ('cronomicon', ?, ?, ?)`,
			name, day, label); err != nil {
			t.Fatalf("seed day %s/%s: %v", name, day, err)
		}
	}
}

func TestLoadResolvesNamedAndGlobalCalendars(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	seedCalendar(t, pool, "holidays", false, false, map[string]string{
		"2026-07-03": "Independence Day (observed)",
		"2026-11-11": "Veterans Day",
	})
	seedCalendar(t, pool, "freeze", true, false, map[string]string{"2026-08-15": "Q3 freeze"})
	seedCalendar(t, pool, "unrelated", false, false, map[string]string{"2026-01-01": "NYD"})
	seedCalendar(t, pool, "weekdays", false, true, nil) // exists, zero days

	sets, err := Load(ctx, pool, []string{"holidays", "weekdays", "holidays", ""})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(sets["holidays"].Days) != 2 {
		t.Errorf("holidays has %d days, want 2", len(sets["holidays"].Days))
	}
	if sets["holidays"].Days["2026-07-03"] != "Independence Day (observed)" {
		t.Errorf("label not loaded: %+v", sets["holidays"])
	}
	// A global calendar is loaded whether or not it was named — that is what
	// makes an entry with no bindings suppressible.
	if _, ok := sets["freeze"]; !ok {
		t.Error("global calendar must load even though it was not named")
	}
	if !sets["freeze"].Global {
		t.Error("global flag not carried through Load")
	}
	if _, ok := sets["unrelated"]; ok {
		t.Error("a non-global, unnamed calendar must NOT be loaded")
	}
	// Present-but-empty must be distinguishable from absent (CAL-16 needs this).
	c, ok := sets["weekdays"]
	if !ok {
		t.Fatal("a calendar with zero days must still appear in the set")
	}
	if len(c.Days) != 0 {
		t.Errorf("weekdays has %d days, want 0", len(c.Days))
	}
	if !c.RecordSuppressed {
		t.Error("record_suppressed flag not carried through Load")
	}
}

// With no names at all, Load still has work to do: the global tier.
func TestLoadWithNoNamesStillLoadsGlobals(t *testing.T) {
	pool := mustPool(t)
	seedCalendar(t, pool, "freeze", true, false, map[string]string{"2026-08-15": ""})
	seedCalendar(t, pool, "holidays", false, false, map[string]string{"2026-07-03": ""})

	sets, err := Load(context.Background(), pool, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("loaded %d calendars, want 1 (globals only)", len(sets))
	}
	if _, ok := sets["freeze"]; !ok {
		t.Error("the global calendar must load with no names requested")
	}
}

// The FK is ON DELETE CASCADE, and migration 860 warns that a future rebuild
// must re-create it. Pin the behaviour so a rebuild that drops it fails here.
func TestCalendarDaysCascadeOnDelete(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedCalendar(t, pool, "holidays", false, false, map[string]string{"2026-07-03": "ID"})

	if _, err := pool.ExecContext(ctx, `DELETE FROM calendars WHERE name='holidays'`); err != nil {
		t.Fatalf("delete calendar: %v", err)
	}
	var n int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_days WHERE calendar_name='holidays'`).Scan(&n); err != nil {
		t.Fatalf("count days: %v", err)
	}
	if n != 0 {
		t.Errorf("%d orphan calendar_days rows survived the cascade, want 0", n)
	}
}
