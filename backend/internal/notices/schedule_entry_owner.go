package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindScheduleEntryNoOwner — a schedule entry that records no owner, under a
// name that more than one definition holds (2.4.0).
//
// An entry of a definition's schedule carries the uid of the definition it
// belongs to. One that carries none (what was left by restoring a schedule
// from the recycle bin before 2.4.0) was joined to its definition by source,
// kind and name, which fired EVERY definition of that name: another agency's
// among them. From 2.4.0 it is joined by name only where one definition holds
// the name, and where two do it fires neither, because whose it is cannot be
// told (scheduler.loadJobEntries). The upgrade leaves such an entry as it is,
// for the same reason (migration 1300).
//
// So on an installation upgraded with two definitions of one name, a schedule
// that fired both stops firing, and this is what says so: the server's log was
// all that did. It names the entry and its timing, and the agencies of the
// definitions that hold the name, to the installation's administrators, who
// are the ones that may read both.
//
// Nothing in the app removes the entry: it is shown on neither definition
// (their details read as the scheduler does), and it fires nothing while two
// hold the name. The administrator puts the schedule on the definition it
// belongs to and dismisses the notice. It clears by itself when the entry is
// gone or one definition is left with the name, which the entry is then
// joined to.
//
// The definitions are counted as the scheduler counts them: every row of the
// name and source, one in the recycle bin too.
//
// Subject: "<job|workflow>:<source>:<definition name>:<entry name>". Filed
// under Global.
const KindScheduleEntryNoOwner = "schedule_entry_no_owner"

func checkScheduleEntryNoOwner(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		WITH holder(kind, source, name, agency) AS (
		    SELECT 'job', j.source, j.name, COALESCE(`+agencyid.JobSQL("j")+`, ?1)
		      FROM jobs j
		  UNION ALL
		    SELECT 'workflow', w.source, w.name, COALESCE(NULLIF(w.owner_agency, ''), ?1)
		      FROM workflows w
		)
		SELECT ds.owner_kind, ds.owner_source, ds.owner_name, ds.name,
		       ds.cron, COALESCE(ds.interval, ''),
		       (SELECT COUNT(*) FROM holder h
		         WHERE h.kind = ds.owner_kind AND h.source = ds.owner_source AND h.name = ds.owner_name),
		       COALESCE((SELECT GROUP_CONCAT(name, ', ') FROM (
		            SELECT DISTINCT COALESCE(a.name, h.agency) AS name
		              FROM holder h LEFT JOIN agencies a ON a.id = h.agency
		             WHERE h.kind = ds.owner_kind AND h.source = ds.owner_source AND h.name = ds.owner_name
		             ORDER BY 1)), '')
		  FROM definition_schedules ds
		 WHERE ds.owner_uid IS NULL
		   AND (SELECT COUNT(*) FROM holder h
		         WHERE h.kind = ds.owner_kind AND h.source = ds.owner_source AND h.name = ds.owner_name) > 1
		 ORDER BY ds.owner_kind, ds.owner_source, ds.owner_name, ds.name`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var kind, source, owner, entry, cron, interval, agencies string
		var holders int
		if err := rows.Scan(&kind, &source, &owner, &entry, &cron, &interval, &holders, &agencies); err != nil {
			rows.Close()
			return err
		}
		timing := "cron " + cron
		if interval != "" {
			timing = "every " + interval
		}
		found = append(found, Finding{
			AgencyID: agencyid.Global,
			Subject:  kind + ":" + source + ":" + owner + ":" + entry,
			Detail: fmt.Sprintf("The schedule entry %q (%s) of the %s %q records no owner, and %d %ss hold that name "+
				"(of: %s). Until 2.4.0 it fired every one of them. It now fires none, because whose it is cannot be told, "+
				"and it is shown on none of them. To put the schedule back, open the %s it belongs to, add the schedule "+
				"there and save; then dismiss this notice. The entry itself stays and fires nothing while more than one %s "+
				"holds the name. This notice clears by itself if one %s is left with the name: the entry is then that one's.",
				entry, timing, kind, owner, holders, kind, agencies, kind, kind, kind),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindScheduleEntryNoOwner, found)
}
