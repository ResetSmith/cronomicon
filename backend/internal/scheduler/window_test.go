package scheduler

import (
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

// AW-3/AW-14 — the column→value adapters the reload path uses. The window
// decorator itself moved into cronutil (ParseSpec applies it for every schedule
// mode), and its firing behavior is covered by cronutil's spec tests.

func at(t *testing.T, v string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return ts
}

func TestParseWindowCols(t *testing.T) {
	// Both NULL ⇒ no window at all.
	if w := parseWindowCols(sql.NullString{}, sql.NullString{}); w != nil {
		t.Fatal("two NULL columns must yield a nil window")
	}
	// A malformed bound is ignored rather than fatal — authoring layers validate,
	// and a bad row must not wedge the scheduler.
	w := parseWindowCols(sql.NullString{String: "not-a-date", Valid: true}, sql.NullString{})
	if w != nil {
		t.Fatalf("unparseable bound must degrade to unbounded, got %+v", w)
	}
	// A good bound parses.
	w = parseWindowCols(sql.NullString{String: "2026-08-05T00:00:00Z", Valid: true}, sql.NullString{})
	if w == nil || !w.Start.Equal(at(t, "2026-08-05T00:00:00Z")) {
		t.Fatalf("valid bound did not parse: %+v", w)
	}
}

func TestWindowLogState(t *testing.T) {
	start := at(t, "2026-08-05T00:00:00Z")
	end := at(t, "2026-09-01T00:00:00Z")
	win := cronutil.NewWindow(&start, &end)

	cases := []struct {
		now  string
		want string
	}{
		{"2026-08-01T00:00:00Z", "pending"},
		{"2026-08-10T00:00:00Z", "active"},
		{"2026-09-10T00:00:00Z", "expired"},
	}
	for _, tc := range cases {
		if got := windowLogState(win, at(t, tc.now)); got != tc.want {
			t.Errorf("at %s: state = %q, want %q", tc.now, got, tc.want)
		}
	}
	if got := windowLogState(nil, time.Now()); got != "" {
		t.Errorf("unbounded entry state = %q, want empty", got)
	}
}
