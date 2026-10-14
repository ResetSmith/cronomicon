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
// steps belong to. One whose jobs are several agencies' is Global's, and from
// 2.4.0 only a global administrator may save it. Such a workflow could be
// saved, until then, by anybody who held compose in each of the agencies.
//
// Nothing stops running. This tells the installation's administrators which
// workflows those are: the ones the upgrade made Global's, and any that comes
// to span later, when a scope is given to another agency. It resolves when
// the workflow's jobs are all one agency's.
//
// The steps are read here as migration 1360 read them, and as the engine
// resolves them: sub-workflows followed, a step's job found by its uid, its
// stated source, or the workflow's own source and then the other.
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
			Detail: fmt.Sprintf("The workflow %s runs jobs of more than one agency (%s), so it belongs to Global. It runs as "+
				"before. From 2.4.0 only a global administrator may save it; until then anybody who held compose in each "+
				"of those agencies could. To give it to one agency, make every job it runs one of that agency's, or split "+
				"it into a workflow for each. This notice clears when its jobs are all one agency's.",
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
		               (SELECT c.rowid FROM workflows c
		                 WHERE c.source IN ('git', 'cronomicon') AND c.name = json_extract(n.value, '$.workflow')
		                   AND c.enabled = 1 AND c.deleted_at IS NULL
		                 ORDER BY (c.source = p.source) DESC, c.rowid LIMIT 1)
		           END,
		           r.depth + 1
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object' AND json_extract(n.value, '$.type') = 'workflow'
		     WHERE r.depth < 3
		),
		stepjob(root, jrow) AS MATERIALIZED (
		    SELECT r.root,
		           CASE WHEN COALESCE(json_extract(n.value, '$.jobUid'), '') <> ''
		             THEN (SELECT j.rowid FROM jobs j
		                    WHERE j.uid = json_extract(n.value, '$.jobUid') AND j.deleted_at IS NULL)
		             ELSE (SELECT j.rowid FROM jobs j
		                    WHERE j.source IN ('git', 'cronomicon') AND j.name = json_extract(n.value, '$.name')
		                      AND j.deleted_at IS NULL
		                      AND (COALESCE(json_extract(n.value, '$.jobSource'), '') = ''
		                           OR j.source = json_extract(n.value, '$.jobSource'))
		                    ORDER BY (j.source = p.source) DESC, j.rowid
		                    LIMIT 1)
		           END
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object'
		       AND COALESCE(json_extract(n.value, '$.type'), '') IN ('', 'job')
		       AND COALESCE(json_extract(n.value, '$.name'), '') <> ''
		),
		step(root, agency) AS MATERIALIZED (
		    SELECT s.root,
		           CASE
		             WHEN COALESCE(j.scope, '') = '' THEN ?1
		             ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                              FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		                             WHERE sc.name = j.scope), ?1)
		           END
		      FROM stepjob s JOIN jobs j ON j.rowid = s.jrow
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
