package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// A project job with no agent that allows checkout (2.4.0; present defects 24,
// 39 and 44).
//
// A script is a PROJECT when it is a directory the agent checks out at a
// pinned commit (project_root). A project job's run is run only by an agent
// started with -allow-checkout, which lists the repository and holds a
// credential for it. Three things changed in 2.4.0, and this notice is where
// an operator is told what they add up to:
//
//   - A job BUILT IN THE APP on a project is a checkout job (present defect
//     24). The composer did not copy the script's marker onto it, and its runs
//     were handed the project's entry file alone, without the roles and files
//     beside it: a playbook that needs them failed, and one that needs nothing
//     ran on any agent that has Ansible.
//   - A project job's run asks for such an agent at the claim (present defect
//     39). Any agent with the run type used to take it, and one that does not
//     allow checkout refused it.
//   - Where no REGISTERED agent that serves the job allows checkout, the run is
//     refused where it is produced (present defect 44), since a run left
//     waiting for an agent that does not exist counts against the
//     installation's concurrency cap for ever.
//
// So a project job with no such agent does not run at all: a scheduled fire is
// recorded as skipped, a manual run is answered checkout_requires_agent, a
// workflow's step fails. This names each such job, from Git or built in the
// app.

// KindProjectJobNoCheckoutAgent — a project job where no registered agent that
// serves the job is started with -allow-checkout, so that its runs are
// refused. Subject: the job's uid. Filed under the job's agency.
const KindProjectJobNoCheckoutAgent = "project_job_no_checkout_agent"

// checkProjectJobs reads the project off the job's own marker and, for a job
// built in the app, off its SCRIPT as well (through jobs.script_uid): at the
// first start of 2.4.0 no job built in the app carries the marker yet, and the
// notice is to be there before the sync or the save that gives it one.
//
// An agent "serves the job" by the rule runref.CheckoutNeedsAgent refuses by:
// it serves the agency of the job's scope (Global, for a job with no scope or
// with a scope the catalogue does not hold) and, where the scope names its
// runners, is one of them. Registered, not
// online: an agent that is merely offline is an ordinary wait. The server's
// own runner is no such agent: it advertises no "checkout" and does not take a
// project job's run. Nor is an agent whose "checkout" an administrator has
// masked: the capabilities are read as the claim reads them
// (execspec.EffectiveCapsSQL), as the refusal reads them.
func checkProjectJobs(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT j.uid, j.name, j.source, COALESCE(j.scope, ''),
		       COALESCE(j.project_root, '') != '',
		       COALESCE((SELECT s.name FROM scripts s WHERE s.uid = j.script_uid), ''),
		       COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                   FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		                  WHERE sc.name = j.scope), ?)
		  FROM jobs j
		 WHERE j.deleted_at IS NULL
		   AND COALESCE(j.uid, '') != ''
		   AND (COALESCE(j.project_root, '') != ''
		        OR (j.source = 'cronomicon'
		            AND EXISTS (SELECT 1 FROM scripts s WHERE s.uid = j.script_uid AND COALESCE(s.project_root, '') != '')))
		   AND NOT EXISTS (
		       SELECT 1 FROM runners rn
		        WHERE rn.kind <> 'server'
		          AND 'checkout' IN `+execspec.EffectiveCapsSQL("rn")+`
		          AND EXISTS (
		              SELECT 1 FROM runner_agencies ra
		               WHERE ra.runner_id = rn.id AND ra.agency_id = rn.owner_agency
		                 AND ((NOT EXISTS (SELECT 1 FROM scopes sc WHERE sc.name = COALESCE(j.scope, '')) AND ra.agency_id = ?)
		                      OR ra.agency_id IN (SELECT sa.agency_id FROM scope_agencies sa
		                                            JOIN scopes sc ON sc.id = sa.scope_id
		                                           WHERE sc.name = j.scope)))
		          AND (NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                            WHERE sc.name = j.scope)
		               OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                           WHERE sc.name = j.scope AND sr.runner_id = rn.id)))
		 ORDER BY j.name, j.uid`, agencyid.Global, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var uid, name, source, scope, script, agency string
		var marked bool
		if err := rows.Scan(&uid, &name, &source, &scope, &marked, &script, &agency); err != nil {
			rows.Close()
			return err
		}
		on := "no scope (it is Global's)"
		if scope != "" {
			on = "the scope " + scope
		}
		what := fmt.Sprintf("The job %s (on %s) is a project", name, on)
		remedy := "change the job in Git to use a script that is not a project"
		if source != "git" {
			what = fmt.Sprintf("The job %s (on %s) was built in the app on the project %s. From 2.4.0 it is a checkout job, as "+
				"a job written in Git on a project has always been; until now its runs were handed the project's entry "+
				"file alone, without the roles and files beside it", name, on, script)
			remedy = "build the job on a script that is not a project"
		}
		// The refusal reads the marker on the job's OWN row
		// (runref.CheckoutNeedsAgent). A job built in the app is named here by
		// its script's marker before its own row carries one, which is from
		// the first start of 2.4.0 until the next sync of its script's
		// repository, or until the job is saved. Until then its runs are not
		// refused: they are what they were, and the words say so.
		refused := "so its runs are refused: a scheduled run is recorded as skipped, a manual one is answered with the " +
			"reason, a workflow's step fails"
		if !marked {
			refused = "so its runs will be refused from the next sync of its script's repository, or when the job is next " +
				"saved (a scheduled run recorded as skipped, a manual one answered with the reason, a workflow's step " +
				"failed). Until then they run as they did before 2.4.0"
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  uid,
			Detail: fmt.Sprintf("%s. A project's run is pinned to a commit and the agent checks the whole project out, "+
				"which only an agent started with -allow-checkout does (it lists the repository and holds a credential "+
				"for it). No registered agent that serves this job is started that way, %s. Either start an agent that "+
				"serves it with -allow-checkout, or %s.", what, refused, remedy),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindProjectJobNoCheckoutAgent, found)
}
