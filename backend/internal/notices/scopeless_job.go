package notices

import (
	"context"
	"database/sql"
	"fmt"
)

// KindJobWithoutScope — an agency's job built in the app has no scope (2.5.3,
// CA7). Since 2.5.3 every job names one of its agency's scopes: the composer
// refuses a job with none, as its agency's repository does (GR-14). One made
// before stays as it was, and its scheduled and automatic starts are refused
// when its script is its agency's (runref.RepoScopeMismatch), so this lists
// them with the way out.
//
// One finding per agency, naming its jobs (the first ten, and how many more).
// Subject: the agency's id. Filed under the agency. Editing a job with no
// scope is a global administrator's (requireComposeScope's empty branch), so
// the detail says so. Central's are listed by central_holds_work, and a job
// in the recycle bin is not listed (purging it is the way out). It clears
// when the agency has none.
const KindJobWithoutScope = "job_without_scope"

func checkJobWithoutScope(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT j.owner_agency, COALESCE(a.name, j.owner_agency), COUNT(*),
		       (SELECT group_concat(name, ', ') FROM (
		            SELECT j2.name AS name FROM jobs j2
		             WHERE j2.owner_agency = j.owner_agency AND j2.source <> 'git' AND j2.deleted_at IS NULL
		               AND TRIM(COALESCE(j2.scope, '')) = ''
		             ORDER BY j2.name LIMIT 10))
		  FROM jobs j LEFT JOIN agencies a ON a.id = j.owner_agency
		 WHERE j.source <> 'git' AND j.deleted_at IS NULL AND TRIM(COALESCE(j.scope, '')) = ''
		   AND COALESCE(j.owner_agency, '') NOT IN ('', 'global')
		 GROUP BY j.owner_agency, a.name
		 ORDER BY j.owner_agency`)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var agency, name string
		var n int
		var jobs sql.NullString
		if err := rows.Scan(&agency, &name, &n, &jobs); err != nil {
			rows.Close()
			return err
		}
		list := jobs.String
		if n > 10 {
			list += fmt.Sprintf(", and %d more", n-10)
		}
		whose := fmt.Sprintf("%d of %s's jobs built in the app have no scope", n, name)
		if n == 1 {
			whose = fmt.Sprintf("One of %s's jobs built in the app has no scope", name)
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  agency,
			Detail: whose + ": " + list + ". Since 2.5.3 every job names one of its agency's scopes, and a job with no scope " +
				"made before is refused when it starts on its own (a schedule, a reaction, a workflow step) if its script is in " +
				"the agency's repository. A global administrator gives each a scope (Jobs → the job → Edit, working in " + name +
				"): for work on the runner itself, a scope whose one host is the runner (an inventory line such as " +
				"\"runner ansible_host=127.0.0.1 ansible_connection=local\"); for a job that pins its target host, a scope that " +
				"lists no hosts. Until then, a person may run one on one of the agency's scopes from the Run dialog.",
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindJobWithoutScope, found)
}
