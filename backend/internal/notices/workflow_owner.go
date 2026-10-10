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
	rows, err := database.QueryContext(ctx, `
		WITH RECURSIVE
		reach(root, wf, depth) AS (
		    SELECT uid, rowid, 0 FROM workflows WHERE source = 'cronomicon' AND deleted_at IS NULL
		  UNION
		    SELECT r.root,
		           (SELECT c.rowid FROM workflows c
		             WHERE c.name = json_extract(n.value, '$.workflow')
		               AND c.enabled = 1 AND c.deleted_at IS NULL
		             ORDER BY (c.source = p.source) DESC, c.rowid LIMIT 1),
		           r.depth + 1
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object' AND json_extract(n.value, '$.type') = 'workflow'
		     WHERE r.depth < 3
		),
		step(root, agency) AS (
		    SELECT r.root,
		           (SELECT CASE
		                     WHEN COALESCE(j.scope, '') = '' THEN ?1
		                     ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                                      FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		                                     WHERE sc.name = j.scope), ?1)
		                   END
		              FROM jobs j
		             WHERE j.deleted_at IS NULL
		               AND CASE
		                     WHEN COALESCE(json_extract(n.value, '$.jobUid'), '') <> ''
		                       THEN j.uid = json_extract(n.value, '$.jobUid')
		                     WHEN COALESCE(json_extract(n.value, '$.jobSource'), '') <> ''
		                       THEN j.name = json_extract(n.value, '$.name') AND j.source = json_extract(n.value, '$.jobSource')
		                     ELSE j.name = json_extract(n.value, '$.name')
		                   END
		             ORDER BY (j.source = p.source) DESC, j.rowid
		             LIMIT 1)
		      FROM reach r
		      JOIN workflows p ON p.rowid = r.wf
		      JOIN json_tree(CASE WHEN json_valid(p.steps) THEN p.steps ELSE '[]' END) n
		        ON n.type = 'object'
		       AND COALESCE(json_extract(n.value, '$.type'), '') IN ('', 'job')
		       AND COALESCE(json_extract(n.value, '$.name'), '') <> ''
		)
		SELECT w.uid, w.name,
		       (SELECT GROUP_CONCAT(name, ', ') FROM (
		            SELECT DISTINCT COALESCE(a.name, s.agency) AS name
		              FROM step s LEFT JOIN agencies a ON a.id = s.agency
		             WHERE s.root = w.uid AND s.agency IS NOT NULL ORDER BY 1))
		  FROM workflows w
		 WHERE w.source = 'cronomicon' AND w.deleted_at IS NULL
		   AND (SELECT COUNT(DISTINCT s.agency) FROM step s WHERE s.root = w.uid AND s.agency IS NOT NULL) > 1
		 ORDER BY w.name`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var uid, name, agencies string
		if err := rows.Scan(&uid, &name, &agencies); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Finding{
			AgencyID: agencyid.Global,
			Subject:  uid,
			Detail: fmt.Sprintf("The workflow %s runs jobs of more than one agency (%s), so it belongs to Global. It runs as "+
				"before. From 2.4.0 only a global administrator may save it; until then anybody who held compose in each "+
				"of those agencies could. To give it to one agency, make every job it runs one of that agency's, or split "+
				"it into a workflow for each. This notice clears when its jobs are all one agency's.",
				name, agencies),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindWorkflowSpansAgencies, found)
}
