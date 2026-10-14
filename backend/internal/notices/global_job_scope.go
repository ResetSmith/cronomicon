package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindGlobalJobOnAgencyScope — a job of Global's repository names a scope
// that is not Global's (2.4.0, §0a, IN-3, open point 1). Until §0a a job of
// Global's repository might name any agency's scope (I-7). Now it runs on
// Global's scopes only: such a job does not run (each run is refused, with
// the reason), and Global's sync keeps it as it was rather than removing it,
// so that what it was is there to copy.
//
// Subject: the job's uid. Filed under Global. It clears when the job's file
// names a scope of Global's, or none, or leaves the repository.
const KindGlobalJobOnAgencyScope = "global_job_on_agency_scope"

func checkGlobalJobOnAgencyScope(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT j.uid, j.name, j.scope,
		       COALESCE((SELECT group_concat(a.name, ', ') FROM scope_agencies sa
		                   JOIN scopes sc ON sc.id = sa.scope_id JOIN agencies a ON a.id = sa.agency_id
		                  WHERE sc.name = j.scope), '')
		  FROM jobs j
		 WHERE j.source = 'git' AND COALESCE(j.repo_id, 'global') = 'global'
		   AND COALESCE(j.scope, '') <> '' AND COALESCE(j.uid, '') <> ''
		   AND NOT EXISTS (SELECT 1 FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		                    WHERE sc.name = j.scope AND sa.agency_id = 'global'
		                      AND (SELECT COUNT(*) FROM scope_agencies x WHERE x.scope_id = sc.id) = 1)
		 ORDER BY j.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var found []Finding
	for rows.Next() {
		var uid, name, scope, agencies string
		if err := rows.Scan(&uid, &name, &scope, &agencies); err != nil {
			return err
		}
		whose := "is not there"
		if agencies != "" {
			whose = "is " + agencies + "'s"
		}
		found = append(found, Finding{
			AgencyID: agencyid.Global,
			Subject:  uid,
			Detail: fmt.Sprintf("The job %s of Global's repository names the scope %s, which %s. A job of Global's "+
				"repository runs on Global's scopes only: this one does not run (each run is refused, with the reason), "+
				"and it is kept as it was. Copy it into that agency's repository, or point it at a scope of Global's. "+
				"This notice clears when it is done.",
				name, scope, whose),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return Reconcile(ctx, database, KindGlobalJobOnAgencyScope, found)
}
