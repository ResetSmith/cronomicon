package gitlab

import "testing"

// AW-8/AW-14 — a window authored in Git must be held to the same contract as one
// authored in-app, and must reach the runtime entry list.
func TestNormalizeWindow(t *testing.T) {
	cases := []struct {
		name             string
		start, end       string
		wantStart, wantE string
		wantErr          bool
	}{
		{name: "both empty"},
		{name: "utc passthrough", start: "2026-08-05T00:00:00Z", wantStart: "2026-08-05T00:00:00Z"},
		{name: "offset normalized to utc", start: "2026-08-05T17:00:00-04:00", wantStart: "2026-08-05T21:00:00Z"},
		{name: "whitespace tolerated", start: "  2026-08-05T00:00:00Z  ", wantStart: "2026-08-05T00:00:00Z"},
		{name: "bad start", start: "next tuesday", wantErr: true},
		{name: "bad end", end: "2026-13-45", wantErr: true},
		{name: "inverted", start: "2026-09-01T00:00:00Z", end: "2026-08-01T00:00:00Z", wantErr: true},
		{name: "equal bounds", start: "2026-09-01T00:00:00Z", end: "2026-09-01T00:00:00Z", wantErr: true},
		{
			name: "ordered pair", start: "2026-08-05T00:00:00Z", end: "2026-09-01T00:00:00Z",
			wantStart: "2026-08-05T00:00:00Z", wantE: "2026-09-01T00:00:00Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStart, gotEnd, err := NormalizeWindow(tc.start, tc.end)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for start=%q end=%q", tc.start, tc.end)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotStart != tc.wantStart || gotEnd != tc.wantE {
				t.Fatalf("got (%q, %q), want (%q, %q)", gotStart, gotEnd, tc.wantStart, tc.wantE)
			}
		})
	}
}

// A window on an inline schedules[] entry survives normalization into the
// canonical entry list the sync writer persists.
func TestNormalizeSchedulesCarriesWindow(t *testing.T) {
	entries, errs := NormalizeSchedules("", []ScheduleEntry{
		{Name: "deferred", Cron: "0 17 * * 3", StartAt: "2026-08-05T17:00:00-04:00"},
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected validation errors: %v", errs)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].StartAt != "2026-08-05T21:00:00Z" {
		t.Fatalf("window not normalized onto the entry: %q", entries[0].StartAt)
	}
}

// An invalid window is a validation error, not a silently-dropped field.
func TestNormalizeSchedulesRejectsBadWindow(t *testing.T) {
	_, errs := NormalizeSchedules("", []ScheduleEntry{
		{Name: "broken", Cron: "0 7 * * *", StartAt: "2026-09-01T00:00:00Z", EndAt: "2026-08-01T00:00:00Z"},
	})
	if len(errs) == 0 {
		t.Fatal("expected a validation error for an inverted window")
	}
}

// Phase 2 — an interval or one-shot entry authored in Git must survive
// normalization with its mode intact, and be held to the same contract as one
// authored in-app.
func TestNormalizeSchedulesModes(t *testing.T) {
	entries, errs := NormalizeSchedules("", []ScheduleEntry{
		{Name: "every-week", Interval: "7d", StartAt: "2026-08-05T17:00:00Z"},
		{Name: "just-once", StartAt: "2026-09-01T00:00:00Z"},
		{Name: "classic", Cron: "0 7 * * *"},
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected validation errors: %v", errs)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].Interval != "7d" || entries[0].Cron != "" {
		t.Errorf("interval entry mangled: %+v", entries[0])
	}
	if entries[1].Cron != "" || entries[1].Interval != "" || entries[1].StartAt == "" {
		t.Errorf("one-shot entry mangled: %+v", entries[1])
	}
	if entries[2].Cron != "0 7 * * *" {
		t.Errorf("cron entry mangled: %+v", entries[2])
	}
}

func TestNormalizeSchedulesRejectsBadModes(t *testing.T) {
	cases := []struct {
		name  string
		entry ScheduleEntry
	}{
		{"cron and interval together", ScheduleEntry{
			Name: "both", Cron: "0 7 * * *", Interval: "7d", StartAt: "2026-08-05T00:00:00Z",
		}},
		{"interval without anchor", ScheduleEntry{Name: "unanchored", Interval: "7d"}},
		{"no rule at all", ScheduleEntry{Name: "empty"}},
		{"sub-minute interval", ScheduleEntry{Name: "toofast", Interval: "10s", StartAt: "2026-08-05T00:00:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, errs := NormalizeSchedules("", []ScheduleEntry{tc.entry}); len(errs) == 0 {
				t.Error("expected a validation error")
			}
		})
	}
}
