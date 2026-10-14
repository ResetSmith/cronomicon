package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// Freeze is the setting.
type Freeze struct {
	On     bool   `json:"on"`
	Reason string `json:"reason"`
	SetBy  string `json:"setBy,omitempty"`
	SetAt  string `json:"setAt,omitempty"`
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

// FreezeVerdict is the verdict of the freeze, when it is on: a skip-polarity
// suppression, always recorded, naming FreezeCalendar and the reason.
func FreezeVerdict(f Freeze) (Verdict, bool) {
	if !f.On {
		return Verdict{}, false
	}
	return Verdict{Suppressed: true, Calendar: FreezeCalendar, Label: f.Reason, Polarity: "skip", Record: true}, true
}
