package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The installation-wide freeze (2.4.0, §0a, open point 2). Until §0a a
// calendar of Global's marked global froze every agency's fires; a calendar is
// its own agency's now, and a freeze of the whole installation is an
// installation setting, set by a global administrator with a reason. While it
// is on it stops every scheduled fire, of every agency, wherever Global's
// global calendar did (the fire, the trigger gates, reactions), and each is
// recorded as a skipped fire with its reason, as a calendar's freeze is.

// FreezeKey is the settings row the freeze is kept in.
const FreezeKey = "freeze.installation"

// FreezeCalendar is what a fire it stopped records as its calendar: the
// suppressed_by_calendar column and the History filter carry a name.
const FreezeCalendar = "installation freeze"

// Freeze is the setting. Since 2.5.2 (FZ-1) it may carry a window: From and
// Until, RFC 3339 in UTC, each optional. No From is "from now"; no Until is
// "until lifted". A freeze with neither is the open switch it always was, which
// is also how a row written before 2.5.2 reads.
type Freeze struct {
	On     bool   `json:"on"`
	Reason string `json:"reason"`
	From   string `json:"from,omitempty"`
	Until  string `json:"until,omitempty"`
	SetBy  string `json:"setBy,omitempty"`
	SetAt  string `json:"setAt,omitempty"`
}

// The states of a freeze at a moment (FZ-4), for the banner and the card: so
// neither does clock arithmetic of its own.
const (
	FreezeOff       = "off"
	FreezeScheduled = "scheduled"
	FreezeInEffect  = "in_effect"
	FreezeEnded     = "ended"
)

// window is the freeze's From and Until as instants; a zero time is an open
// end. An unparseable bound reads as open: the setting is validated when it
// is written, and a damaged row must not end a freeze on its own.
func (f Freeze) window() (from, until time.Time) {
	if t, err := time.Parse(time.RFC3339, f.From); err == nil {
		from = t
	}
	if t, err := time.Parse(time.RFC3339, f.Until); err == nil {
		until = t
	}
	return from, until
}

// FreezeState is the freeze's state at t: off, scheduled (on, and t before
// From), in effect (from <= t < until), or ended (t at or after Until).
func FreezeState(f Freeze, t time.Time) string {
	if !f.On {
		return FreezeOff
	}
	from, until := f.window()
	switch {
	case !from.IsZero() && t.Before(from):
		return FreezeScheduled
	case !until.IsZero() && !t.Before(until):
		return FreezeEnded
	}
	return FreezeInEffect
}

// ReadFreeze reads the setting; off when it was never set.
func ReadFreeze(ctx context.Context, database *sql.DB) (Freeze, error) {
	var raw string
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, FreezeKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Freeze{}, nil
	}
	if err != nil {
		return Freeze{}, fmt.Errorf("read the installation freeze: %w", err)
	}
	var f Freeze
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return Freeze{}, fmt.Errorf("read the installation freeze: %w", err)
	}
	return f, nil
}

// FreezeVerdictAt is the verdict of the freeze for a fire at t (FZ-2): when
// it is in effect then, a skip-polarity suppression, always recorded, naming
// FreezeCalendar and the reason. Judged at the moment in question rather than
// by a timer, so a window begins and ends on time with nothing to wake up for
// it, and a server that was down over either edge is right at its next fire.
// A future fire is judged at its own time (FZ-3).
func FreezeVerdictAt(f Freeze, t time.Time) (Verdict, bool) {
	if FreezeState(f, t) != FreezeInEffect {
		return Verdict{}, false
	}
	return Verdict{Suppressed: true, Calendar: FreezeCalendar, Label: f.Reason, Polarity: "skip", Record: true}, true
}
