package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindScheduleBindingAmbiguous — a job or workflow takes its timing from a
// reusable schedule by NAME, and its entry is tied to no schedule although a
// schedule of that name exists (2.4.0, migration 1300).
//
// A definition bound to a reusable schedule holds a copy of the schedule's
// timing, the schedule's name, and since 1300 the schedule's uid, which is what
// an edit of the schedule, its deletion and its "used by" list find the entry
// by. The upgrade filled the uid wherever it could tell which schedule an entry
// came from. It could not for an in-app definition whose name is held by two
// schedules (Git's and one built in the app): the entry was expanded from
// whichever the composer found when the definition was last saved, and that is
// not recorded. It was left without a uid, not guessed.
//
// The same holds for an entry whose uid names a schedule that has since gone,
// when another schedule now holds the name: a Git schedule removed from its
// repository leaves the in-app definitions that were bound to it holding its
// uid, and an operator who then builds a schedule of that name in the app
// expects it to govern them. Until 2.4.0 it did, by the name.
//
// Such an entry is not broken. It fires at the timing it has. But it follows
// neither schedule: an edit of either does not reach it, and neither counts the
// definition among its users. Saving the definition again settles it (the
// composer binds a live schedule of the name, the in-app one before Git's) and
// this notice resolves.
//
// Subject: "<job|workflow>:<owner uid>:<entry name>". Filed under the agency of
// a job's scope when the scope is in exactly one, otherwise under Global; a
// workflow's is Global's.
const KindScheduleBindingAmbiguous = "schedule_binding_ambiguous"

func checkScheduleBindingAmbiguous(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT ds.owner_kind,
		       COALESCE(NULLIF(ds.owner_uid, ''), ds.owner_source || ':' || ds.owner_name),
		       ds.owner_name, ds.owner_source, ds.name, ds.source_ref, ds.cron,
		       (SELECT COUNT(*) FROM schedules s WHERE s.name = ds.source_ref AND s.deleted_at IS NULL),
		       COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                   FROM jobs j
		                   JOIN scopes sc         ON sc.name = j.scope
		                   JOIN scope_agencies sa ON sa.scope_id = sc.id
		                  WHERE ds.owner_kind = 'job' AND j.uid = ds.owner_uid), ?)
		  FROM definition_schedules ds
		 -- tied to no schedule: no uid, or the uid of a schedule that has gone
		 -- (a Git schedule removed from its repository leaves the in-app
		 -- definitions bound to it holding its uid)
		 WHERE (ds.schedule_uid IS NULL
		        OR NOT EXISTS (SELECT 1 FROM schedules s0 WHERE s0.uid = ds.schedule_uid))
		   AND COALESCE(ds.source_ref, '') <> ''
		   -- a schedule of that name is there to be bound to; an entry whose
		   -- schedule has gone and has no namesake is a different condition
		   AND EXISTS (SELECT 1 FROM schedules s WHERE s.name = ds.source_ref AND s.deleted_at IS NULL)
		   -- the owner is live: a binned definition fires nothing. An entry with
		   -- no owner uid (what is left under a name two definitions share) is
		   -- matched to its owner the way the scheduler matches it, by the name.
		   AND (   (ds.owner_kind = 'job'      AND EXISTS (SELECT 1 FROM jobs j
		                 WHERE j.deleted_at IS NULL
		                   AND (j.uid = ds.owner_uid
		                        OR (ds.owner_uid IS NULL AND j.source = ds.owner_source AND j.name = ds.owner_name))))
		        OR (ds.owner_kind = 'workflow' AND EXISTS (SELECT 1 FROM workflows w
		                 WHERE w.deleted_at IS NULL
		                   AND (w.uid = ds.owner_uid
		                        OR (ds.owner_uid IS NULL AND w.source = ds.owner_source AND w.name = ds.owner_name)))))
		 ORDER BY ds.owner_kind, ds.owner_name, ds.name`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var kind, ownerKey, ownerName, ownerSource, entry, ref, cron, agency string
		var live int
		if err := rows.Scan(&kind, &ownerKey, &ownerName, &ownerSource, &entry, &ref, &cron, &live, &agency); err != nil {
			rows.Close()
			return err
		}
		which := "A schedule of that name exists"
		if live > 1 {
			which = fmt.Sprintf("%d schedules have that name (one from Git, one built in the app)", live)
		}
		how := "Open the " + kind + " and save it"
		if ownerSource == "git" {
			how = "Sync the repository the " + kind + " comes from"
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  kind + ":" + ownerKey + ":" + entry,
			Detail: fmt.Sprintf("The %s %s takes the timing %q from a reusable schedule called %s, and its entry is tied to "+
				"no schedule that exists. %s, and which one this %s is bound to is not recorded: either it was saved "+
				"before 2.4.0, when a definition was tied to its schedule by the name alone, or the schedule it was bound "+
				"to has since been removed. The entry was left as it is and not guessed. It still fires, at %s, but it "+
				"follows neither schedule: an edit of the schedule does not reach it, and the schedule does not list the %s "+
				"among its users. %s: it is bound to a live schedule of that name, the one built in the app before Git's, "+
				"and this notice clears. To bind it to the other one, rename or remove the schedule you do not want first.",
				kind, ownerName, entry, ref, which, kind, cron, kind, how),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindScheduleBindingAmbiguous, found)
}
