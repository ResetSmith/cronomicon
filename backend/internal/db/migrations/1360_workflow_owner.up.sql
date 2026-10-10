-- 1360_workflow_owner — a workflow belongs to one agency (2.4.0, GR-6, GR-7;
-- Phase R4).
--
-- A schedule has had an owner since 1300. A workflow had none: whose it is was
-- worked out from its steps' jobs each time somebody asked. With a repository
-- per agency a Git workflow's owner is a fact (its repository's agency), and
-- an in-app workflow's is the one agency its job steps belong to.
--
--   * A Git workflow: its repository's agency.
--   * A workflow built in the app: the agency of the jobs its steps run, when
--     they are all one agency's. Sub-workflows are followed, as the engine
--     follows them (by name, in the parent's source and then the other, the
--     oldest live one), to the depth it follows them.
--   * A workflow whose jobs are several agencies' is Global's (GR-7). So is
--     one that runs a job with no scope, a job whose scope is Global's or is
--     shared by several agencies, and one that runs no job at all.
--
-- Nothing is refused here and nothing stops running. A workflow that became
-- Global's because its steps span agencies is listed in the inbox
-- (workflow_spans_agencies, a check, so it also catches one that comes to span
-- later, when a scope is given to another agency). From now on the composer
-- writes the owner each time a workflow is saved.
--
-- A job step is found as the engine finds it: by `jobUid` when the step pins
-- one, otherwise by name in the step's `jobSource`, or in the workflow's own
-- source and then the other. A step whose job is not there says nothing about
-- whose the workflow is.

ALTER TABLE workflows ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';

UPDATE workflows
   SET owner_agency = COALESCE((SELECT g.agency_id FROM git_repos g WHERE g.id = workflows.repo_id), 'global')
 WHERE source = 'git';

WITH RECURSIVE
reach(root, wf, depth) AS (
    SELECT uid, rowid, 0 FROM workflows WHERE source = 'cronomicon'
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
                     WHEN COALESCE(j.scope, '') = '' THEN 'global'
                     ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
                                      FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
                                     WHERE sc.name = j.scope), 'global')
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
UPDATE workflows
   SET owner_agency = COALESCE((SELECT CASE WHEN COUNT(DISTINCT s.agency) = 1 THEN MIN(s.agency) ELSE 'global' END
                                  FROM step s WHERE s.root = workflows.uid AND s.agency IS NOT NULL), 'global')
 WHERE source = 'cronomicon';
