package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// A job built in the app on a project is a checkout job since 2.4.0 (present
// defect 24). A script is a PROJECT when it is a directory the agent checks
// out at a pinned commit (scripts.project_root). A job written in Git on such
// a script has always been a checkout job: sync copies the marker onto it.
// The composer did not copy it onto a job built in the app, whose runs were
// handed the project's entry file alone, without the roles and files beside
// it: a playbook that needs them failed, and one that needs nothing ran on any
// agent that has Ansible. The composer copies the marker now, and so does the
// next sync for a job that is already there.
//
// What that changes for a job that ran: a checkout run is run only by an agent
// started with -allow-checkout, which lists the repository and holds a
// credential for it. On any other agent it is taken and refused, and the run
// fails. This says so, for each such job that has no agent of that kind.

// KindInAppProjectJobNoCheckoutAgent — a job built in the app on a project,
// where no registered agent that serves the job is started with
// -allow-checkout. Subject: the job's uid. Filed under the job's agency.
const KindInAppProjectJobNoCheckoutAgent = "inapp_project_job_no_checkout_agent"

// checkInAppProjectJobs reads the project off the SCRIPT (through
// jobs.script_uid) and not off the job's own copy: at the first start of
// 2.4.0 no job built in the app carries the marker yet, and the notice is to
// be there before the sync or the save that gives it one, not after.
//
// An agent "serves the job" by the claim's rule: it serves the agency of the
// job's scope (Global, for a job with no scope) and, where the scope names its
// runners, is one of them. Registered, not online: an agent that is merely
// offline is an ordinary wait.
//
// Nor is it raised where the SERVER's own runner serves the job and has its run
// type (a shell-type project: the local runner takes no other). The local
// runner runs such a job as a body and does not honour the pin, so "its runs
// will fail" would not be true of it. That it runs so is a defect of its own
// (the plan's R5 record, "Read, and not changed"), and not this notice's to
// describe.
func checkInAppProjectJobs(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT j.uid, j.name, COALESCE(j.scope, ''), s.name,
		       COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                   FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		                  WHERE sc.name = j.scope), ?)
		  FROM jobs j
		  JOIN scripts s ON s.uid = j.script_uid
		 WHERE j.source = 'cronomicon'
		   AND j.deleted_at IS NULL
		   AND COALESCE(j.uid, '') != ''
		   AND COALESCE(s.project_root, '') != ''
		   AND NOT EXISTS (
		       SELECT 1 FROM runners rn
		        WHERE ((rn.kind <> 'server'
		                AND EXISTS (SELECT 1 FROM json_each(COALESCE(rn.capabilities, '[]')) c WHERE c.value = 'checkout'))
		               OR (rn.kind = 'server'
		                   AND EXISTS (SELECT 1 FROM json_each(COALESCE(rn.capabilities, '[]')) c WHERE c.value = j.run_type)))
		          AND EXISTS (
		              SELECT 1 FROM runner_agencies ra
		               WHERE ra.runner_id = rn.id
		                 AND ((COALESCE(j.scope, '') = '' AND ra.agency_id = ?)
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
		var uid, name, scope, script, agency string
		if err := rows.Scan(&uid, &name, &scope, &script, &agency); err != nil {
			rows.Close()
			return err
		}
		on := "no scope (it is Global's)"
		if scope != "" {
			on = "the scope " + scope
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  uid,
			Detail: fmt.Sprintf("The job %s (on %s) was built in the app on the project %s. From 2.4.0 it is a checkout job, as "+
				"a job written in Git on a project has always been: each run is pinned to a commit and the agent checks "+
				"the whole project out. Until now its runs were handed the project's entry file alone, without the roles "+
				"and files beside it. A checkout run is run only by an agent started with -allow-checkout, which lists "+
				"the repository and holds a credential for it, and no registered agent that serves this job is started "+
				"that way: its runs will be taken by an agent that refuses them, and will fail. Either start an agent "+
				"that serves it with -allow-checkout, or build the job on a script that is not a project.",
				name, on, script),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindInAppProjectJobNoCheckoutAgent, found)
}
