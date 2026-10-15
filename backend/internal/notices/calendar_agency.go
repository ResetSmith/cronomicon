package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
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

func checkCalendarAgencies(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `SELECT name FROM calendars WHERE owner_agency = ? AND global = 1 ORDER BY name`, agencyid.Global)
	if err != nil {
		return err
	}
	var globals []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		globals = append(globals, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var others int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agencies WHERE id <> ?`, agencyid.Global).Scan(&others); err != nil {
		return err
	}
	var scope []Finding
	if others > 0 {
		for _, c := range globals {
			scope = append(scope, Finding{
				AgencyID: agencyid.Global,
				Subject:  c,
				Detail: fmt.Sprintf("The calendar %s is global and Global's. Until 2.4.0 a global calendar of Global's "+
					"froze every agency's fires; now a calendar freezes its own agency's only, so it freezes Global's. "+
					"To stop every agency's fires, use the installation-wide freeze (Settings); to give agencies these "+
					"days, give each a copy of the calendar. This notice clears when the calendar is not global.", c),
			})
		}
	}
	return Reconcile(ctx, database, KindGlobalCalendarScope, scope)
}
