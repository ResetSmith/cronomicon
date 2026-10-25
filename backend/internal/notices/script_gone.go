package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindJobScriptGone — a job built in the app whose script is no longer there
// (2.4.0; present defects 41 and 45, the owner's decision of 2026-10-13).
//
// A job built in the app runs its script as the server last synced it from Git
// (execspec.ResolveCommand reads the script's record at every run). A script
// that is removed from its repository, or renamed there, leaves the jobs built
// on it joined to nothing, and such a job does not run: its run is refused
// with "the script this job is built on has been removed". Until 2.4.0 it went
// on running the copy it held, and nothing said that the script had gone.
//
// It resolves when a script of that name is there for the job again (the next
// sync joins it, and says so), or the job is saved on another script, or the
// job is removed. Subject: the job's uid. Filed under the agency of the job's
// scope when the scope is in exactly one; otherwise under Global.
const KindJobScriptGone = "job_script_gone"

// checkJobScriptGone is execspec's scriptGoneSQL, asked of every job built in
// the app that is not in the recycle bin.
func checkJobScriptGone(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT j.uid, j.name, COALESCE(j.scope, ''), COALESCE(j.script_ref, ''),
		       COALESCE(NULLIF(j.owner_agency, ''), ?) -- its owner (1400)
		  FROM jobs j
		 WHERE j.source <> 'git'
		   AND j.deleted_at IS NULL
		   AND COALESCE(j.uid, '') != ''
		   AND (COALESCE(j.script_ref, '') <> ''
		        OR (COALESCE(j.command, '') = '' AND COALESCE(j.script, '') = '' AND COALESCE(j.script_path, '') <> ''))
		   AND NOT EXISTS (SELECT 1 FROM scripts s WHERE s.uid = j.script_uid)
		 ORDER BY j.name, j.uid`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var uid, name, scope, script, agency string
		if err := rows.Scan(&uid, &name, &scope, &script, &agency); err != nil {
			rows.Close()
			return err
		}
		on := "no scope (its runs are its agency's)"
		if scope != "" {
			on = "the scope " + scope
		}
		built := "a script that is no longer there"
		if script != "" {
			built = "the script " + script + ", which is no longer there"
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  uid,
			Detail: fmt.Sprintf("The job %s (on %s) was built in the app on %s: it was removed from its repository, or "+
				"renamed there. A job built in the app runs its script as Git has it, so this job does not run: each "+
				"run is refused with \"the script this job is built on has been removed\". Until 2.4.0 it went on "+
				"running the copy it held. Either put the script back under that name (the next sync joins the job to "+
				"it again), or edit the job to use a script that is there.", name, on, built),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindJobScriptGone, found)
}
