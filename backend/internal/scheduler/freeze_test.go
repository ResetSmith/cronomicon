package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// A global calendar freezes its own agency's fires only (§0a, 2.4.0); the
// installation-wide freeze stops every agency's, with its reason. Until §0a a
// global calendar of anybody's froze every fire, and there was no setting.
func TestI1_AGlobalCalendarFreezesItsAgencyAndTheFreezeEveryone(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	const day = "2026-12-25"
	for _, c := range [][2]string{{"g-freeze", "global"}, {"fin-freeze", "ag-fin"}} {
		exec(`INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES ('cronomicon', ?, 1, 0, 't', 't', ?)`, c[0], c[1])
		exec(`INSERT INTO calendar_days (calendar_owner, calendar_source, calendar_name, day, label) VALUES (?, 'cronomicon', ?, ?, 'holiday')`, c[1], c[0], day)
	}
	verdict := func(agency string) calendar.Verdict {
		t.Helper()
		v, err := freezeTier(ctx, pool, agency, nil, nil, day, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for agency, want := range map[string]string{"global": "g-freeze", "ag-fin": "fin-freeze", "ag-tax": ""} {
		v := verdict(agency)
		if v.Calendar != want || v.Suppressed != (want != "") {
			t.Errorf("a fire of %s on the day: suppressed=%v by %q, want %q", agency, v.Suppressed, v.Calendar, want)
		}
	}
	exec(`INSERT INTO settings (key, value) VALUES (?, '{"on":true,"reason":"year-end"}')`, calendar.FreezeKey)
	for _, agency := range []string{"global", "ag-fin", "ag-tax"} {
		v := verdict(agency)
		if !v.Suppressed || v.Calendar != calendar.FreezeCalendar || calendar.Reason(v) != "Skipped: the installation is frozen (year-end)" {
			t.Errorf("a fire of %s while frozen: %+v (%q)", agency, v, calendar.Reason(v))
		}
	}
}

// 2.5.2 FZ-2: the freeze is judged at the fire's moment, by the same tier
// every producer asks (the scheduled fire, the reactor, the gate for runs,
// deferred runs and arrivals): before a window and after it a fire goes on,
// inside it the fire is skipped with the reason.
func TestFZ_AFireIsJudgedAtItsMoment(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	from := time.Date(2026, 12, 24, 18, 0, 0, 0, time.UTC)
	until := from.Add(60 * time.Hour)
	if _, err := pool.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, calendar.FreezeKey,
		`{"on":true,"reason":"year-end","from":"`+from.Format(time.RFC3339)+`","until":"`+until.Format(time.RFC3339)+`"}`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		at     time.Time
		frozen bool
	}{
		{"before", from.Add(-time.Minute), false},
		{"at from", from, true},
		{"inside", from.Add(30 * time.Hour), true},
		{"at until", until, false},
	} {
		v, err := freezeTier(ctx, pool, "global", nil, nil, calendar.DayOf(c.at, time.UTC), c.at)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.Suppressed && v.Calendar == calendar.FreezeCalendar; got != c.frozen {
			t.Errorf("a fire %s the window: frozen=%v, want %v (%+v)", c.name, got, c.frozen, v)
		}
	}
}
