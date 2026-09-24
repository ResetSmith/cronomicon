package cronutil_test

import (
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

func TestValid(t *testing.T) {
	cases := []struct {
		expr  string
		valid bool
	}{
		{"*/5 * * * *", true},  // 5-field
		{"0 30 9 * * *", true}, // 6-field (with seconds)
		{"0 9 * * *", true},    // 5-field daily
		{"@hourly", true},      // descriptor
		{"", false},            // empty
		{"Manual", false},      // sentinel, not a cron
		{"not a cron", false},  // garbage
		{"60 * * * *", false},  // minute out of range
	}
	for _, c := range cases {
		if got := cronutil.Valid(c.expr); got != c.valid {
			t.Errorf("Valid(%q) = %v, want %v", c.expr, got, c.valid)
		}
	}
}

// TestFallbackEquivalence proves a 5-field expression parsed via Parse fires at
// exactly the same instants as its explicit 6-field "0 "+expr form — i.e. the
// seconds-prepend fallback preserves semantics.
func TestFallbackEquivalence(t *testing.T) {
	base := time.Date(2026, 3, 15, 8, 0, 0, 0, time.Local)
	five, err := cronutil.Parse("30 9 * * *")
	if err != nil {
		t.Fatalf("parse 5-field: %v", err)
	}
	six, err := cronutil.Parse("0 30 9 * * *")
	if err != nil {
		t.Fatalf("parse 6-field: %v", err)
	}
	if a, b := five.Next(base), six.Next(base); !a.Equal(b) {
		t.Errorf("5-field next %v != 6-field next %v", a, b)
	}
}

func TestNext(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 15, 0, 0, time.Local)
	next, ok := cronutil.Next("0 * * * *", base, time.Local) // top of every hour
	if !ok {
		t.Fatal("expected ok for valid expr")
	}
	want := time.Date(2026, 1, 1, 11, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("Next = %v, want %v", next, want)
	}
	if _, ok := cronutil.Next("bogus", base, time.Local); ok {
		t.Error("expected ok=false for invalid expr")
	}
}

// TestNextHonorsLocation proves the evaluation zone is honored: "2 a.m. daily"
// resolves to a different absolute UTC instant under America/New_York vs UTC —
// the whole point of the app-zone change (timezone-update §8).
func TestNextHonorsLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	// A fixed UTC instant just after midnight UTC on a winter day (EST = UTC-5).
	base := time.Date(2026, 1, 15, 0, 30, 0, 0, time.UTC)

	gotUTC, ok := cronutil.Next("0 2 * * *", base, time.UTC)
	if !ok {
		t.Fatal("expected ok (UTC)")
	}
	gotNY, ok := cronutil.Next("0 2 * * *", base, ny)
	if !ok {
		t.Fatal("expected ok (NY)")
	}
	// 02:00 UTC vs 02:00 EST (=07:00 UTC) — same wall-clock rule, different instant.
	wantUTC := time.Date(2026, 1, 15, 2, 0, 0, 0, time.UTC)
	wantNY := time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC)
	if !gotUTC.Equal(wantUTC) {
		t.Errorf("UTC Next = %v, want %v", gotUTC.UTC(), wantUTC)
	}
	if !gotNY.Equal(wantNY) {
		t.Errorf("NY Next = %v, want %v", gotNY.UTC(), wantNY)
	}
	if gotUTC.Equal(gotNY) {
		t.Error("expected different instants for UTC vs America/New_York; zone not honored")
	}
}

func TestNextN(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)

	// Horizon bounds the result: hourly, horizon 2h30m → 11:00 and 12:00 only.
	got := cronutil.NextN("0 * * * *", base, base.Add(150*time.Minute), 10, time.Local)
	if len(got) != 2 {
		t.Fatalf("horizon-bounded NextN = %d times, want 2 (%v)", len(got), got)
	}

	// n caps the result when the horizon is far, and times are soonest-first.
	got = cronutil.NextN("0 * * * *", base, base.Add(72*time.Hour), 3, time.Local)
	if len(got) != 3 {
		t.Fatalf("n-capped NextN = %d, want 3", len(got))
	}
	if !got[0].Before(got[1]) || !got[1].Before(got[2]) {
		t.Errorf("NextN not ascending: %v", got)
	}

	// Invalid expression or non-positive n yields nil.
	if cronutil.NextN("bogus", base, base.Add(time.Hour), 5, time.Local) != nil {
		t.Error("expected nil for invalid expr")
	}
	if cronutil.NextN("0 * * * *", base, base.Add(time.Hour), 0, time.Local) != nil {
		t.Error("expected nil for n=0")
	}
}
