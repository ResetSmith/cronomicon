package cronutil

import (
	"testing"
	"time"
)

// AW-2/AW-14 — the window-aware projections must agree with what the scheduler
// actually fires, since the UI's "next run" and "upcoming" views are built from
// them. Every case here is a claim about firing behavior, not just formatting.

func mustTime(t *testing.T, v string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return ts
}

// nextWindowed is the single-fire projection through the one windowed
// entry point production uses (NextNWindowed, n=1). The exported NextWindowed
// was deleted in the DD band — nothing outside tests called it — but the four
// firing claims below are about clampAfter/past, which NextNWindowed shares.
func nextWindowed(expr string, after time.Time, loc *time.Location, win *Window) (time.Time, bool) {
	out := NextNWindowed(expr, after, after.AddDate(10, 0, 0), 1, loc, win)
	if len(out) == 0 {
		return time.Time{}, false
	}
	return out[0], true
}

// The motivating case: "5pm every 7 days starting Wed 8/5" authored on 8/3.
func TestNextWindowedDefersToStart(t *testing.T) {
	now := mustTime(t, "2026-08-03T12:00:00Z")
	start := mustTime(t, "2026-08-05T00:00:00Z")
	win := NewWindow(new(start), nil)

	next, ok := nextWindowed("0 17 * * 3", now, time.UTC, win)
	if !ok {
		t.Fatal("expected a projected fire")
	}
	if want := mustTime(t, "2026-08-05T17:00:00Z"); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

// A cron match landing exactly on the window start must fire, not be skipped by
// the strictly-after contract.
func TestNextWindowedFiresExactlyAtStart(t *testing.T) {
	now := mustTime(t, "2026-08-01T00:00:00Z")
	start := mustTime(t, "2026-08-05T17:00:00Z")
	win := NewWindow(new(start), nil)

	next, ok := nextWindowed("0 17 * * *", now, time.UTC, win)
	if !ok {
		t.Fatal("expected a projected fire")
	}
	if !next.Equal(start) {
		t.Fatalf("next = %v, want the window start %v", next, start)
	}
}

// AW-Q2: a start already in the past is inert — identical to no window at all.
// No catch-up, no backfill, no difference in the projection.
func TestNextWindowedPastStartIsInert(t *testing.T) {
	now := mustTime(t, "2026-08-10T12:00:00Z")
	past := mustTime(t, "2026-08-05T00:00:00Z")

	windowed, ok1 := nextWindowed("0 17 * * *", now, time.UTC, NewWindow(new(past), nil))
	bare, ok2 := Next("0 17 * * *", now, time.UTC)
	if !ok1 || !ok2 {
		t.Fatal("expected both projections to yield a fire")
	}
	if !windowed.Equal(bare) {
		t.Fatalf("past start changed the projection: %v vs unbounded %v", windowed, bare)
	}
}

// An elapsed end bound silences the entry entirely.
func TestNextWindowedExpired(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	end := mustTime(t, "2026-09-01T00:00:00Z")

	if _, ok := nextWindowed("0 17 * * *", now, time.UTC, NewWindow(nil, new(end))); ok {
		t.Fatal("expired window must project no fire")
	}
}

// A fire beyond the end bound truncates the sequence rather than being clamped
// back into range.
func TestNextNWindowedStopsAtEnd(t *testing.T) {
	now := mustTime(t, "2026-08-01T00:00:00Z")
	end := mustTime(t, "2026-08-04T00:00:00Z")
	horizon := now.Add(30 * 24 * time.Hour)

	got := NextNWindowed("0 12 * * *", now, horizon, 10, time.UTC, NewWindow(nil, new(end)))
	// Daily noon fires on the 1st, 2nd, 3rd; the 4th's noon is past the bound.
	if len(got) != 3 {
		t.Fatalf("got %d fires (%v), want 3 bounded by end", len(got), got)
	}
	for _, f := range got {
		if f.After(end) {
			t.Fatalf("fire %v is past the window end %v", f, end)
		}
	}
}

// Both bounds together: nothing before the start, nothing after the end.
func TestNextNWindowedBothBounds(t *testing.T) {
	now := mustTime(t, "2026-08-01T00:00:00Z")
	start := mustTime(t, "2026-08-05T00:00:00Z")
	end := mustTime(t, "2026-08-08T00:00:00Z")
	horizon := now.Add(60 * 24 * time.Hour)

	got := NextNWindowed("0 12 * * *", now, horizon, 20, time.UTC, NewWindow(new(start), new(end)))
	if len(got) == 0 {
		t.Fatal("expected fires inside the window")
	}
	for _, f := range got {
		if f.Before(start) || f.After(end) {
			t.Fatalf("fire %v escaped the window [%v, %v]", f, start, end)
		}
	}
}

// A nil window must behave exactly like the unbounded helpers — this is what
// keeps every pre-existing schedule on its original code path.
func TestNilWindowMatchesUnbounded(t *testing.T) {
	now := mustTime(t, "2026-08-03T12:00:00Z")
	horizon := now.Add(7 * 24 * time.Hour)

	if got, want := NextNWindowed("0 7 * * *", now, horizon, 5, time.UTC, nil),
		NextN("0 7 * * *", now, horizon, 5, time.UTC); len(got) != len(want) {
		t.Fatalf("nil window yielded %d fires, unbounded yielded %d", len(got), len(want))
	}
}

func TestWindowStateHelpers(t *testing.T) {
	start := mustTime(t, "2026-08-05T00:00:00Z")
	end := mustTime(t, "2026-09-01T00:00:00Z")
	win := NewWindow(new(start), new(end))

	before := mustTime(t, "2026-08-01T00:00:00Z")
	inside := mustTime(t, "2026-08-10T00:00:00Z")
	after := mustTime(t, "2026-09-10T00:00:00Z")

	if !win.Pending(before) || win.Active(before) {
		t.Error("before the start the window must read pending and inactive")
	}
	if !win.Active(inside) || win.Pending(inside) || win.Expired(inside) {
		t.Error("inside the window must read active only")
	}
	if !win.Expired(after) || win.Active(after) {
		t.Error("after the end the window must read expired and inactive")
	}

	// A nil window is always active and never pending/expired.
	var none *Window
	if !none.Active(inside) || none.Pending(inside) || none.Expired(inside) {
		t.Error("a nil window must read as unconditionally active")
	}
}

// AW-18 — "@every" is an unanchored interval this system does not express as a
// cron; accepting it through the API/YAML while every UI surface rejects it was
// the leak this closes. Calendar descriptors stay valid.
func TestEveryDescriptorRejected(t *testing.T) {
	for _, expr := range []string{"@every 168h", "@EVERY 1h", "  @every 30m  "} {
		if Valid(expr) {
			t.Errorf("%q must be rejected (unanchored interval)", expr)
		}
	}
	for _, expr := range []string{"@daily", "@weekly", "@hourly", "@midnight"} {
		if !Valid(expr) {
			t.Errorf("%q must stay valid (calendar descriptor with a cron equivalent)", expr)
		}
	}
}
