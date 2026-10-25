package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindGlobalScriptJob — an agency's job (built in the app, or, since 2.4.2, of
// its repository) is joined to a script of Global's repository (2.4.0, §0a,
// IN-8; the owner's yes to question 31). Until §0a an agency with no script of
// the name in its own repository found it in Global's, and Global's sync
// carried its edits to the job (a Git job's too, until 2.4.2). Now a
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
		 WHERE s.repo_id = ? AND COALESCE(j.uid, '') <> '' AND j.deleted_at IS NULL
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
	central := agencyid.CurrentGlobalName(ctx, database) // the built-in agency's name now (2.5.2 GR)
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
			// 2.5.0 C11, defect 90 said a run on no scope ran as Global's; since
			// 2.5.3 (CA-5) such a run is its job's agency's (execspec.ScopeAgencies),
			// and only a run on a scope is confined (runref.RepoScopeMismatch).
			Detail: fmt.Sprintf("The job %s is built on the script %s of %s's repository. %s runs nothing since 2.5.3, and a "+
				"definition takes nothing from another agency's repository: its runs on this agency's scopes are refused, "+
				"with the reason. Copy the script into this agency's repository and choose it for the job. This notice "+
				"clears when it is done.", j.name, j.script, central, central),
		})
	}
	return Reconcile(ctx, database, KindGlobalScriptJob, found)
}
