package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindGlobalScheduleBound — a job or workflow of an agency is bound to a
// schedule of Global's (2.4.0, §0a, IN-8; the owner's answer to question 32).
// Until §0a every schedule built in the app was Global's and any agency's
// definition bound to it, and Global's schedules (built in the app or of
// Global's repository) carried their edits to every definition bound to them.
// Now a schedule's edits reach its own agency's definitions only: this one
// keeps the timing it copied, and fires on it, but no longer follows the
// schedule.
//
// Subject: `<definition uid>:<entry name>`. Filed under the definition's
// agency. It clears when the entry is bound to a schedule of its own agency's,
// given a timing of its own, or removed.
const KindGlobalScheduleBound = "global_schedule_bound"

func checkGlobalScheduleBound(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT d.owner_uid, d.owner_kind, d.owner_name, d.name, s.name, d.cron
		  FROM definition_schedules d JOIN schedules s ON s.uid = d.schedule_uid
		 WHERE s.owner_agency = ? AND COALESCE(d.owner_uid, '') <> ''
		 ORDER BY d.owner_name, d.name`, agencyid.Global)
	if err != nil {
		return err
	}
	type entry struct{ uid, kind, owner, name, schedule, cron string }
	var all []entry
	for rows.Next() {
		var e entry
		var cron sql.NullString
		if err := rows.Scan(&e.uid, &e.kind, &e.owner, &e.name, &e.schedule, &cron); err != nil {
			rows.Close()
			return err
		}
		e.cron = cron.String
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	central := agencyid.CurrentGlobalName(ctx, database) // the built-in agency's name now (2.5.2 GR)
	var found []Finding
	for _, e := range all {
		var agency sql.NullString
		if err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, e.uid, e.kind).Scan(&agency); err != nil {
			return err
		}
		if !agency.Valid || agency.String == agencyid.Global {
			continue
		}
		found = append(found, Finding{
			AgencyID: agency.String,
			Subject:  e.uid + ":" + e.name,
			Detail: fmt.Sprintf("The %s %s is bound to %s's schedule %s. A schedule's edits reach its own agency's "+
				"definitions only: this one keeps the timing it copied (%s) and fires on it, and no longer follows the "+
				"schedule. Bind it to a schedule of this agency's, or give it a timing of its own. This notice clears "+
				"when it is done.", e.kind, e.owner, central, e.schedule, e.cron),
		})
	}
	return Reconcile(ctx, database, KindGlobalScheduleBound, found)
}
