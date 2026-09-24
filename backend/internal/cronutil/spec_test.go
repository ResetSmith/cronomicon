package cronutil

import (
	"errors"
	"testing"
	"time"
)

// Phase 2 — anchored intervals and one-shots. These assert on firing behavior:
// ParseSpec's result is what the engine registers, so a wrong Next() here is a
// job running at the wrong time.

func TestParseIntervalForms(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"10 d", 10 * 24 * time.Hour},
		{"36h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"  7D  ", 7 * 24 * time.Hour},
		{"", 0},
	}
	for _, tc := range cases {
		got, err := ParseInterval(tc.in)
		if err != nil {
			t.Errorf("ParseInterval(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseInterval(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// A sub-minute interval is far likelier a typo than an intent, and would
	// spin the engine faster than a run can complete.
	for _, bad := range []string{"30s", "0d", "-5h", "next tuesday", "7 days", "d"} {
		if _, err := ParseInterval(bad); err == nil {
			t.Errorf("ParseInterval(%q) should have failed", bad)
		}
	}
}

// The motivating Phase 2 case: "every 10 days from Aug 5" — unrepresentable in
// cron, since cron has no anchor to phase against.
func TestIntervalSchedulePhasesFromAnchor(t *testing.T) {
	anchor := mustTime(t, "2026-08-05T17:00:00Z")
	sp := Spec{Interval: "10d", Window: NewWindow(new(anchor), nil)}

	// Before the anchor, the first fire IS the anchor.
	next, ok := NextSpec(sp, mustTime(t, "2026-08-01T00:00:00Z"), time.UTC)
	if !ok || !next.Equal(anchor) {
		t.Fatalf("first fire = %v (ok=%v), want the anchor %v", next, ok, anchor)
	}

	// Then every 10 days, phased on the anchor rather than the calendar.
	got := NextNSpec(sp, mustTime(t, "2026-08-05T17:00:01Z"), anchor.Add(60*24*time.Hour), 3, time.UTC)
	want := []time.Time{
		mustTime(t, "2026-08-15T17:00:00Z"),
		mustTime(t, "2026-08-25T17:00:00Z"),
		mustTime(t, "2026-09-04T17:00:00Z"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d fires (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("fire %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// A query landing exactly on a fire instant advances to the next one
// (strictly-after), so a fire is never returned twice.
func TestIntervalScheduleStrictlyAfter(t *testing.T) {
	anchor := mustTime(t, "2026-08-05T00:00:00Z")
	sched := intervalSchedule{anchor: anchor, every: 24 * time.Hour}

	if next := sched.Next(anchor); !next.Equal(anchor.Add(24 * time.Hour)) {
		t.Fatalf("Next at the anchor = %v, want one interval later", next)
	}
	at := anchor.Add(72 * time.Hour)
	if next := sched.Next(at); !next.Equal(at.Add(24 * time.Hour)) {
		t.Fatalf("Next on a fire instant = %v, want the following fire", next)
	}
}

// An interval schedule still honors its window's end bound.
func TestIntervalScheduleStopsAtEnd(t *testing.T) {
	anchor := mustTime(t, "2026-08-05T00:00:00Z")
	end := mustTime(t, "2026-08-20T00:00:00Z")
	sp := Spec{Interval: "7d", Window: NewWindow(new(anchor), new(end))}

	got := NextNSpec(sp, mustTime(t, "2026-08-01T00:00:00Z"), anchor.Add(365*24*time.Hour), 10, time.UTC)
	// Aug 5, 12, 19 fire; Aug 26 is past the end.
	if len(got) != 3 {
		t.Fatalf("got %d fires (%v), want 3 bounded by end", len(got), got)
	}
	for _, f := range got {
		if f.After(end) {
			t.Fatalf("fire %v escaped the end bound %v", f, end)
		}
	}
}

// One-shot: an anchor with no cron and no interval fires exactly once.
func TestOnceSchedule(t *testing.T) {
	at := mustTime(t, "2026-08-05T17:00:00Z")
	sp := Spec{Window: NewWindow(new(at), nil)}

	next, ok := NextSpec(sp, mustTime(t, "2026-08-01T00:00:00Z"), time.UTC)
	if !ok || !next.Equal(at) {
		t.Fatalf("one-shot fire = %v (ok=%v), want %v", next, ok, at)
	}
	// After it has fired, never again.
	if _, ok := NextSpec(sp, at.Add(time.Second), time.UTC); ok {
		t.Fatal("a one-shot must not fire twice")
	}
	if n := NextNSpec(sp, mustTime(t, "2026-08-01T00:00:00Z"), at.Add(365*24*time.Hour), 5, time.UTC); len(n) != 1 {
		t.Fatalf("one-shot projected %d fires, want exactly 1", len(n))
	}
}

func TestSpecModeAndValidation(t *testing.T) {
	anchor := mustTime(t, "2026-08-05T00:00:00Z")
	withAnchor := NewWindow(new(anchor), nil)

	cases := []struct {
		name     string
		spec     Spec
		wantMode string
		wantErr  error
		anyErr   bool
	}{
		{name: "cron", spec: Spec{Cron: "0 7 * * *"}, wantMode: ModeCron},
		{name: "cron with window", spec: Spec{Cron: "0 7 * * *", Window: withAnchor}, wantMode: ModeCron},
		{name: "interval", spec: Spec{Interval: "7d", Window: withAnchor}, wantMode: ModeInterval},
		{name: "once", spec: Spec{Window: withAnchor}, wantMode: ModeOnce},
		{
			name: "both modes", spec: Spec{Cron: "0 7 * * *", Interval: "7d", Window: withAnchor},
			wantMode: ModeCron, wantErr: ErrSpecConflict,
		},
		{name: "interval without anchor", spec: Spec{Interval: "7d"}, wantMode: ModeInterval, wantErr: ErrIntervalNeedsAnchor},
		{name: "nothing at all", spec: Spec{}, wantMode: ModeOnce, wantErr: ErrSpecEmpty},
		{name: "bad cron", spec: Spec{Cron: "not a cron"}, wantMode: ModeCron, anyErr: true},
		{name: "bad interval", spec: Spec{Interval: "30s", Window: withAnchor}, wantMode: ModeInterval, anyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Mode(); got != tc.wantMode {
				t.Errorf("Mode() = %q, want %q", got, tc.wantMode)
			}
			err := tc.spec.Validate()
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Validate() = %v, want %v", err, tc.wantErr)
				}
			case tc.anyErr:
				if err == nil {
					t.Error("Validate() should have failed")
				}
			default:
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			}
		})
	}
}

// A cron spec must behave exactly as the Phase 1 windowed path did — Phase 2
// routes every mode through ParseSpec, so this pins that the refactor changed
// no cron timing.
func TestParseSpecCronMatchesWindowedProjection(t *testing.T) {
	now := mustTime(t, "2026-08-01T00:00:00Z")
	start := mustTime(t, "2026-08-05T00:00:00Z")
	horizon := now.Add(30 * 24 * time.Hour)
	win := NewWindow(new(start), nil)

	viaSpec := NextNSpec(Spec{Cron: "0 12 * * *", Window: win}, now, horizon, 5, time.UTC)
	viaWindow := NextNWindowed("0 12 * * *", now, horizon, 5, time.UTC, win)
	if len(viaSpec) != len(viaWindow) {
		t.Fatalf("spec path gave %d fires, windowed path %d", len(viaSpec), len(viaWindow))
	}
	for i := range viaSpec {
		if !viaSpec[i].Equal(viaWindow[i]) {
			t.Errorf("fire %d: spec %v vs windowed %v", i, viaSpec[i], viaWindow[i])
		}
	}
}

func TestDescribeSpec(t *testing.T) {
	anchor := mustTime(t, "2026-08-05T00:00:00Z")
	win := NewWindow(new(anchor), nil)
	cases := map[string]Spec{
		"cron 0 7 * * *": {Cron: "0 7 * * *"},
		"every 7d":       {Interval: "7d", Window: win},
		"once":           {Window: win},
	}
	for want, sp := range cases {
		if got := DescribeSpec(sp); got != want {
			t.Errorf("DescribeSpec = %q, want %q", got, want)
		}
	}
}
