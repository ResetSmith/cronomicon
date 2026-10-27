package notices

import (
	"context"
	"database/sql"
)

// (calendar_other_agency, which listed an agency's entry naming another
// agency's calendar between 1390 and I2, went with migration 1430: each such
// agency has a copy of the calendar under the same name now, and an entry
// finds its calendars in its own agency.)

// KindGlobalCalendarScope — a calendar of Global's is global (2.4.0, §0a):
// until then it froze every agency's fires; now it freezes Global's only, and
// a freeze of the whole installation is the installation-wide freeze. Raised
// while there is an agency besides Global. Subject: the calendar's name. Filed
// under Global. It clears when the calendar is not global, or goes.
const KindGlobalCalendarScope = "global_calendar_scope"

// Retired in 2.5.3 (CA-18): Global's calendars are the catalogue agencies take copies of (CA-25), and a calendar marked global freezes only its own agency since FZ. Open notices clear at the next check.
func checkCalendarAgencies(ctx context.Context, database *sql.DB) error {
	return Reconcile(ctx, database, KindGlobalCalendarScope, nil)
}
