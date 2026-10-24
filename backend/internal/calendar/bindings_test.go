package calendar

import (
	"strings"
	"testing"
)

// 2.5.3 CA-31: one rule for a definition's own calendars and its entry's. A
// Forbid at either level, or a freeze calendar of the agency, vetoes and is
// recorded; each level that names Allow calendars must list the day (a union
// within a level, both levels when both name some); a Forbid prevails over an
// Allow listing the same day, and says so.
func TestCA4b_TheLevelsCombine(t *testing.T) {
	const day = "2026-12-24"
	sets := Set{
		"holidays": {Name: "holidays", Owner: "ag", Days: DaySet{day: "Christmas Eve"}},
		"close":    {Name: "close", Owner: "ag", Days: DaySet{day: ""}},
		"q4":       {Name: "q4", Owner: "ag", Days: DaySet{"2026-12-01": ""}},
		"weekdays": {Name: "weekdays", Owner: "ag", Days: DaySet{day: ""}},
		"freeze":   {Name: "freeze", Owner: "ag", Global: true, Days: DaySet{day: "Freeze"}},
		"other":    {Name: "other", Owner: "ag", Days: DaySet{}, RecordSuppressed: true},
	}
	noFreeze := Set{}
	for k, v := range sets {
		if k != "freeze" {
			noFreeze[k] = v
		}
	}
	cases := []struct {
		name               string
		set                Set
		b                  Bindings
		suppressed         bool
		calendar, polarity string
		level              string
		record             bool
	}{
		{"the job's Forbid", noFreeze, Bindings{Kind: "job", DefForbid: []string{"holidays"}}, true, "holidays", "skip", "job", true},
		{"the entry's skip", noFreeze, Bindings{Kind: "job", EntrySkip: []string{"holidays"}}, true, "holidays", "skip", "", true},
		{"the agency's freeze vetoes an Allow", sets, Bindings{Kind: "job", DefAllow: []string{"close"}}, true, "freeze", "skip", "", true},
		{"the job's Allow lists the day", noFreeze, Bindings{Kind: "job", DefAllow: []string{"close"}}, false, "", "", "", false},
		{"a union within a level", noFreeze, Bindings{Kind: "job", DefAllow: []string{"q4", "close"}}, false, "", "", "", false},
		{"the job's Allow misses", noFreeze, Bindings{Kind: "workflow", DefAllow: []string{"q4"}}, true, "q4", "only", "workflow", false},
		{"recorded when its calendar opts in", noFreeze, Bindings{Kind: "job", DefAllow: []string{"other"}}, true, "other", "only", "job", true},
		{"both levels pass", noFreeze, Bindings{Kind: "job", DefAllow: []string{"close"}, EntryOnly: []string{"weekdays"}}, false, "", "", "", false},
		{"the entry passes, the job does not", noFreeze, Bindings{Kind: "job", DefAllow: []string{"q4"}, EntryOnly: []string{"weekdays"}}, true, "q4", "only", "job", false},
		{"the job passes, the entry does not", noFreeze, Bindings{Kind: "job", DefAllow: []string{"close"}, EntryOnly: []string{"q4"}}, true, "q4", "only", "", false},
	}
	for _, c := range cases {
		v := EvaluateBindings(c.set, "ag", c.b, day)
		if v.Suppressed != c.suppressed || v.Calendar != c.calendar || v.Polarity != c.polarity || v.Level != c.level || v.Record != c.record {
			t.Errorf("%s: %+v, want suppressed %v by %q (%s, level %q, record %v)", c.name, v, c.suppressed, c.calendar, c.polarity, c.level, c.record)
		}
	}

	// A conflict: the job allows the day and forbids it. The Forbid prevails,
	// and the reason says why.
	v := EvaluateBindings(noFreeze, "ag", Bindings{Kind: "job", DefForbid: []string{"holidays"}, DefAllow: []string{"close"}}, day)
	if !v.Suppressed || v.Calendar != "holidays" || v.AlsoAllowedBy != "close" {
		t.Fatalf("a Forbid and an Allow on one day: %+v, want suppressed by holidays, close noted", v)
	}
	r := Reason(v)
	for _, want := range []string{`the job's calendar "holidays"`, "Christmas Eve", `"close"`, "Forbid prevails"} {
		if !strings.Contains(r, want) {
			t.Errorf("the reason %q does not say %q", r, want)
		}
	}
	// An entry's calendars alone read as before 2.5.3.
	if r := Reason(EvaluateFor(noFreeze, "ag", []string{"holidays"}, nil, day)); r != `Skipped: suppressed by calendar "holidays" (Christmas Eve)` {
		t.Errorf("an entry's skip reads %q, as before", r)
	}
}

// The review of CA4b: an entry's skip against its own only calendar reads as
// it did before 2.5.3; the conflict note is for a definition's calendars.
func TestCA4b_AnEntrysOwnConflictReadsAsBefore(t *testing.T) {
	const day = "2026-12-25"
	sets := Set{
		"holidays": {Name: "holidays", Owner: "ag", Days: DaySet{day: "Christmas"}},
		"weekdays": {Name: "weekdays", Owner: "ag", Days: DaySet{day: ""}},
	}
	v := EvaluateFor(sets, "ag", []string{"holidays"}, []string{"weekdays"}, day)
	if v.AlsoAllowedBy != "" || Reason(v) != `Skipped: suppressed by calendar "holidays" (Christmas)` {
		t.Errorf("an entry's skip over its only: %+v %q", v, Reason(v))
	}
	// The job's own Allow listing the day the entry forbids is noted.
	if v := EvaluateBindings(sets, "ag", Bindings{Kind: "job", DefAllow: []string{"weekdays"}, EntrySkip: []string{"holidays"}}, day); v.AlsoAllowedBy != "weekdays" {
		t.Errorf("the job's Allow over the entry's skip: %+v", v)
	}
}
