package calendar

import (
	"testing"
	"time"
)

// 2.5.2 FZ-1 to FZ-4: a freeze may carry a window, judged at the moment in
// question. From is inclusive, Until exclusive; an open end is open; a row
// written before 2.5.2 (no window) is the open switch it was.
func TestFZ_TheFreezeIsJudgedAtTheMomentInQuestion(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	window := Freeze{On: true, Reason: "year-end", From: "2026-12-24T18:00:00Z", Until: "2026-12-27T06:00:00Z"}
	for _, c := range []struct {
		name  string
		f     Freeze
		t     string
		state string
	}{
		{"before the window", window, "2026-12-24T17:59:59Z", FreezeScheduled},
		{"at from (inclusive)", window, "2026-12-24T18:00:00Z", FreezeInEffect},
		{"inside", window, "2026-12-25T12:00:00Z", FreezeInEffect},
		{"a second before until", window, "2026-12-27T05:59:59Z", FreezeInEffect},
		{"at until (exclusive)", window, "2026-12-27T06:00:00Z", FreezeEnded},
		{"no from: from now", Freeze{On: true, Until: "2026-12-27T06:00:00Z"}, "2020-01-01T00:00:00Z", FreezeInEffect},
		{"no until: until lifted", Freeze{On: true, From: "2026-12-24T18:00:00Z"}, "2099-01-01T00:00:00Z", FreezeInEffect},
		{"a 2.5.1 row: open", Freeze{On: true, Reason: "old"}, "2026-12-25T12:00:00Z", FreezeInEffect},
		{"off", Freeze{On: false, From: "2026-12-24T18:00:00Z"}, "2026-12-25T12:00:00Z", FreezeOff},
	} {
		if got := FreezeState(c.f, at(c.t)); got != c.state {
			t.Errorf("%s: state %q, want %q", c.name, got, c.state)
		}
		v, frozen := FreezeVerdictAt(c.f, at(c.t))
		if frozen != (c.state == FreezeInEffect) {
			t.Errorf("%s: frozen %v in state %q", c.name, frozen, c.state)
		}
		if frozen && (v.Calendar != FreezeCalendar || v.Label != c.f.Reason || !v.Suppressed || !v.Record) {
			t.Errorf("%s: verdict %+v", c.name, v)
		}
	}
}
