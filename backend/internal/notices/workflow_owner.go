package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindWorkflowSpansAgencies — a workflow built in the app runs jobs of more
// than one agency (2.4.0, GR-7).
//
// A workflow belongs to one agency, like everything else: the one its job
// steps belong to. One whose jobs are several agencies' is Global's (the
// upgrade, migration 1360, made it so), and since §0a (IN-8) nobody may save
// one: its steps of any other agency fail when it runs, each with the reason
// (§0a, open point 1). Until §0a a global administrator might save it, and
// before 2.4.0 anybody who held compose in each of the agencies.
//
// This tells the installation's administrators which workflows those are:
// the ones the upgrade made Global's, and any that comes to span later, when
// a scope is given to another agency. It resolves when the workflow's jobs
// are all one agency's.
//
// The steps are read here as the engine resolves them (2.4.2, present defect
// 59: until then by the lowest rowid of a name, across repositories):
// sub-workflows followed, a step's job found by its uid, else in its stated
// source or the workflow's own and then the other, a Git workflow's within
// its repository and its agency, and a name two jobs hold as nothing.
//
// Subject: the workflow's uid. Filed under Global: the workflow is Global's,
// and the detail names the agencies it reaches.
const KindWorkflowSpansAgencies = "workflow_spans_agencies"

func checkWorkflowSpansAgencies(ctx context.Context, database *sql.DB) error {
	spanning, err := SpanningWorkflows(ctx, database)
	if err != nil {
		return err
	}
	var found []Finding
	for _, w := range spanning {
		found = append(found, Finding{
			AgencyID: agencyid.Global,
			Subject:  w.UID,
			Detail: fmt.Sprintf("The workflow %s runs jobs of more than one agency (%s). A workflow runs its own "+
				"agency's jobs only: when it runs, each step on a scope that is not its agency's fails, with the reason, "+
				"and it cannot be saved as it is. Copy what it needs into one agency, or split it into a workflow for each. "+
				"This notice clears when its jobs are all one agency's.",
				w.Name, w.Agencies),
		})
	}
	return Reconcile(ctx, database, KindWorkflowSpansAgencies, found)
}

// SpanningWorkflow is a workflow built in the app whose jobs are several
// agencies' (GR-7): its uid, its name, and the agencies' names.
type SpanningWorkflow struct {
	UID, Name, Agencies string
}

// SpanningWorkflows lists the workflows built in the app that run jobs of more
// than one agency, as they are on q: the database, or a transaction with a
// save not yet committed in it, which is how a save asks whether it would
// make a workflow span (the API's refuseNewSpan). One reading for the notice
// and for that refusal.
func SpanningWorkflows(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) ([]SpanningWorkflow, error) {
	// The graph is read ONCE (2.4.0, review 5). Every save by anybody but a
	// global administrator runs this twice inside its write transaction, so
	// its cost is a lock every other writer waits on. Written first with the
	// per-step rows as a plain CTE that the final SELECT asked of each
	// workflow, it re-read the whole graph per workflow: 200 workflows with a
	// sub-workflow each took five seconds, 2,000 eight minutes. Now the steps'
	// jobs, their agencies and the count per workflow are each MATERIALIZED
	// once, and a step's job and a sub-workflow are found through the
	// (source, name) index (a name alone does not use it; a source is one of
	// the two there are).
	rows, err := q.QueryContext(ctx, `
		WITH RECURSIVE
		reach(root, wf, depth) AS (
		    SELECT uid, rowid, 0 FROM workflows WHERE source = 'cronomicon' AND deleted_at IS NULL
		  UNION
		    SELECT r.root,
		           -- The child as the engine finds it (loadChildWorkflow, 2.4.0):
		           -- by the step's workflowUid when it pins one; a parent built in
		           -- the app that pins none, the one workflow built in the app of
		           -- the name, else Global's repository's; a Git parent, its own
		           -- source's first. One that is binned or disabled is no child,
		           -- and is not replaced by another of its name.
		           CASE
		             WHEN COALESCE(json_extract(n.value, '$.workflowUid'), '') <> '' THEN
		               (SELECT CASE WHEN c.enabled = 1 AND c.deleted_at IS NULL THEN c.rowid END
		                  FROM workflows c WHERE c.uid = json_extract(n.value, '$.workflowUid'))
		             WHEN p.source = 'cronomicon' THEN
		               CASE
		                 WHEN (SELECT COUNT(*) FROM workflows c
		                        WHERE c.source = 'cronomicon' AND c.name = json_extract(n.value, '$.workflow')) > 0 THEN
		                   (SELECT CASE WHEN COUNT(*) = 1 AND MAX(c.enabled = 1 AND c.deleted_at IS NULL) THEN MIN(c.rowid) END
		                      FROM workflows c
		                     WHERE c.source = 'cronomicon' AND c.name = json_extract(n.value, '$.workflow'))
		                 ELSE
		                   (SELECT CASE WHEN c.enabled = 1 AND c.deleted_at IS NULL THEN c.rowid END
		                      FROM workflows c
		                     WHERE c.source = 'git' AND c.name = json_extract(n.value, '$.workflow')
		                       AND COALESCE(c.repo_id, 'global') = 'global')
		               END
		             ELSE
		               -- A Git parent: its own repository's workflow of the name (one
		               -- that is off fails the step), else a workflow built in the app
		               -- of its repository's agency (loadChildWorkflow; 2.4.2, present
		               -- defect 59: this took the lowest rowid of the name, any repository).
		               COALESCE(
		                 (SELECT CASE WHEN c.enabled = 1 AND c.deleted_at IS NULL THEN c.rowid ELSE -1 END
		                    FROM workflows c
		                   WHERE c.source = 'git' AND c.name = json_extract(n.value, '$.workflow')
		                     AND COALESCE(c.repo_id, 'global') = COALESCE(p.repo_id, 'global')),
		                 (SELECT MIN(c.rowid) FROM workflows c
		                   WHERE c.source = 'cronomicon' AND c.name = json_extract(n.value, '$.workflow')
		                     AND c.enabled = 1 AND c.deleted_at IS NULL
		                     AND c.owner_agency = (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(p.repo_id, 'global'))))
		           END,
		           r.depth + 1
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object' AND json_extract(n.value, '$.type') = 'workflow'
		     WHERE r.depth < 3
		),
		-- A job step of each reached workflow, and the sources its job is looked
		-- for in, in the engine's order (StepSourceOrder): the step's own, else
		-- the workflow's and then the other.
		stepref(root, prow, name, juid, src, ord) AS MATERIALIZED (
		    SELECT r.root, p.rowid, json_extract(n.value, '$.name'), COALESCE(json_extract(n.value, '$.jobUid'), ''),
		           CASE WHEN COALESCE(json_extract(n.value, '$.jobSource'), '') <> '' THEN json_extract(n.value, '$.jobSource') ELSE p.source END,
		           1
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object'
		       AND COALESCE(json_extract(n.value, '$.type'), '') IN ('', 'job')
		       AND COALESCE(json_extract(n.value, '$.name'), '') <> ''
		  UNION ALL
		    SELECT r.root, p.rowid, json_extract(n.value, '$.name'), COALESCE(json_extract(n.value, '$.jobUid'), ''),
		           CASE WHEN p.source = 'cronomicon' THEN 'git' ELSE 'cronomicon' END,
		           2
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object'
		       AND COALESCE(json_extract(n.value, '$.type'), '') IN ('', 'job')
		       AND COALESCE(json_extract(n.value, '$.name'), '') <> ''
		       AND COALESCE(json_extract(n.value, '$.jobSource'), '') = ''
		),
		-- The job found in each source, as resolveJobDef finds it (2.4.2, present
		-- defect 59: this was the lowest rowid of the name across every
		-- repository and agency). A Git workflow's: its own repository's job, then
		-- its repository's agency's jobs built in the app. Otherwise the one job of
		-- the name in that source. Binned and disabled rows are found (they stop
		-- the search, as in the engine); a name two jobs hold is -1, ambiguous,
		-- and the step does not run.
		stepfound(root, prow, name, ord, jrow) AS MATERIALIZED (
		    SELECT s.root, s.prow, s.name, s.ord,
		           CASE
		             WHEN s.src = 'git' AND p.source = 'git' THEN
		               (SELECT j.rowid FROM jobs j WHERE j.source = 'git' AND j.name = s.name
		                   AND COALESCE(j.repo_id, 'global') = COALESCE(p.repo_id, 'global'))
		             WHEN s.src = 'cronomicon' AND p.source = 'git' THEN
		               (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(j.rowid) WHEN COUNT(*) > 1 THEN -1 END
		                  FROM jobs j WHERE j.source = 'cronomicon' AND j.name = s.name
		                   AND COALESCE(j.owner_agency, 'global') = (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(p.repo_id, 'global')))
		             ELSE
		               (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(j.rowid) WHEN COUNT(*) > 1 THEN -1 END
		                  FROM jobs j WHERE j.source = s.src AND j.name = s.name)
		           END
		      FROM stepref s JOIN workflows p ON p.rowid = s.prow
		     WHERE s.juid = ''
		),
		stepjob(root, jrow) AS MATERIALIZED (
		    SELECT root, (SELECT j.rowid FROM jobs j WHERE j.uid = juid AND j.deleted_at IS NULL)
		      FROM (SELECT DISTINCT root, juid FROM stepref WHERE juid <> '')
		  UNION ALL
		    SELECT f.root, (SELECT f2.jrow FROM stepfound f2
		                     WHERE f2.root = f.root AND f2.prow = f.prow AND f2.name = f.name AND f2.jrow IS NOT NULL
		                     ORDER BY f2.ord LIMIT 1)
		      FROM (SELECT DISTINCT root, prow, name FROM stepfound) f
		),
		step(root, agency) AS MATERIALIZED (
		    SELECT s.root,
		           CASE
		             WHEN COALESCE(j.scope, '') = '' THEN ?1
		             ELSE COALESCE(`+agencyid.JobSQL("j")+`, ?1) -- its owner (1400)
		           END
		      FROM stepjob s JOIN jobs j ON j.rowid = s.jrow AND j.deleted_at IS NULL
		),
		counted(root, n) AS MATERIALIZED (
		    SELECT root, COUNT(DISTINCT agency) FROM step GROUP BY root
		)
		SELECT w.uid, w.name,
		       (SELECT GROUP_CONCAT(name, ', ') FROM (
		            SELECT DISTINCT COALESCE(a.name, s.agency) AS name
		              FROM step s LEFT JOIN agencies a ON a.id = s.agency
		             WHERE s.root = w.uid ORDER BY 1))
		  FROM workflows w
		  JOIN counted c ON c.root = w.uid AND c.n > 1
		 WHERE w.source = 'cronomicon' AND w.deleted_at IS NULL
		 ORDER BY w.name`, agencyid.Global)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpanningWorkflow
	for rows.Next() {
		var w SpanningWorkflow
		if err := rows.Scan(&w.UID, &w.Name, &w.Agencies); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
