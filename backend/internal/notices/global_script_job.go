package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindGlobalScriptJob — a job built in the app by an agency is joined to a
// script of Global's repository (2.4.0, §0a, IN-8; the owner's yes to question
// 31). Until §0a an agency with no script of the name in its own repository
// found it in Global's, and Global's sync carried its edits to the job. Now a
// definition takes nothing from Global's: the job's runs are refused, with the
// reason, until it is built on a script of its own agency's repository.
//
// Subject: the job's uid. Filed under the job's agency. It clears when the
// job's script is its own agency's, or the job goes.
const KindGlobalScriptJob = "global_script_job"

func checkGlobalScriptJob(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT j.uid, j.name, s.name
		  FROM jobs j JOIN scripts s ON s.uid = j.script_uid
		 WHERE j.source = 'cronomicon' AND s.repo_id = ? AND COALESCE(j.uid, '') <> '' AND j.deleted_at IS NULL
		 ORDER BY j.name`, agencyid.Global)
	if err != nil {
		return err
	}
	type job struct{ uid, name, script string }
	var all []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.uid, &j.name, &j.script); err != nil {
			rows.Close()
			return err
		}
		all = append(all, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var found []Finding
	for _, j := range all {
		var agency sql.NullString
		if err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, j.uid, "job").Scan(&agency); err != nil {
			return err
		}
		if !agency.Valid || agency.String == agencyid.Global {
			continue
		}
		found = append(found, Finding{
			AgencyID: agency.String,
			Subject:  j.uid,
			Detail: fmt.Sprintf("The job %s is built on the script %s of Global's repository. A definition takes nothing "+
				"from Global's: its runs are refused, with the reason. Copy the script into this agency's repository "+
				"and choose it for the job. This notice clears when it is done.", j.name, j.script),
		})
	}
	return Reconcile(ctx, database, KindGlobalScriptJob, found)
}
