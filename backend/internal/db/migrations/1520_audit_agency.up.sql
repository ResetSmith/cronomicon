-- 1520_audit_agency — every audit row and every workflow run records the
-- agency it is of (2.5.0 C1; CF-3, CF-4, CF-6).
--
-- An agency reads its own audit rows and no other's (CF-1), so the agency of a
-- row must be in the row: worked out when it is read (2.4.4's Activity, by the
-- run, the workflow run, then the job) it cannot be filtered on, and a change
-- log row could not be placed at all. The OBJECT's agency, never the agency the
-- request was viewing (the owner's answer to question 74): 2.4.4 stamped
-- activity.agency_id from the request, and C1's writers replace that.
--
-- workflow_runs.agency_id is the workflow's when the run is created (CF-6:
-- its old `scope` column is never written). A trigger stamps it, as 1480's does
-- a run's owner, so that every insert is covered; one that names an agency
-- keeps it (a refused child run is its parent's, which the engine names).
--
-- The backfill (CF-4) takes only what an old row can be attributed by:
--   activity:   its run's owner, its workflow run's agency, its job's, its
--               workflow's (by uid), then the agency 2.4.4 stored, where no
--               object key resolves (decision D7);
--   change_log: a target that is a run or a workflow run (secrets injected,
--               a workflow cancelled), a workflow trigger's "workflow run
--               <trace>", an agency's membership ("agency members: <id>").
-- Everything else is Global's: names are per agency since I2, so a name
-- cannot say whose. change_log.acting_agency is the agency VIEWED, not the
-- object's, and is never a source (D6). Reactions' depth-ceiling rows carry
-- the upstream run's trace, which is not their object (D5): they are left to
-- the agency 2.4.4 stored, else Global's.
--
-- A NULL agency_id after this migration is a row whose writer named nothing:
-- it reads as Global's (agencyid.Owner) and is a writer's defect to fix.

ALTER TABLE workflow_runs ADD COLUMN agency_id TEXT;
ALTER TABLE change_log ADD COLUMN agency_id TEXT;

UPDATE workflow_runs SET agency_id = COALESCE(
    (SELECT (CASE WHEN w.source = 'git'
                  THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                  ELSE w.owner_agency END)
       FROM workflows w WHERE w.uid = workflow_runs.workflow_uid),
    (SELECT (CASE WHEN w.source = 'git'
                  THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                  ELSE w.owner_agency END)
       FROM workflows w
      WHERE w.source = COALESCE(workflow_runs.workflow_source, 'git') AND w.name = workflow_runs.workflow_name
      ORDER BY w.rowid LIMIT 1),
    'global');

CREATE TRIGGER workflow_runs_agency_stamped AFTER INSERT ON workflow_runs
WHEN NEW.agency_id IS NULL
BEGIN
    UPDATE workflow_runs SET agency_id = COALESCE(
        (SELECT (CASE WHEN w.source = 'git'
                      THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                      ELSE w.owner_agency END)
           FROM workflows w WHERE w.uid = NEW.workflow_uid),
        (SELECT (CASE WHEN w.source = 'git'
                      THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                      ELSE w.owner_agency END)
           FROM workflows w
          WHERE w.source = COALESCE(NEW.workflow_source, 'git') AND w.name = NEW.workflow_name
          ORDER BY w.rowid LIMIT 1),
        'global')
     WHERE rowid = NEW.rowid;
END;

UPDATE activity SET agency_id = CASE
  WHEN activity.category = 'Reactions' AND activity.action = 'Depth ceiling'
  THEN COALESCE(NULLIF(activity.agency_id, ''), 'global')
  ELSE COALESCE(
    NULLIF((SELECT ru.owner_agency FROM runs ru WHERE ru.id = activity.trace_id), ''),
    NULLIF((SELECT wr.agency_id FROM workflow_runs wr WHERE wr.id = activity.trace_id), ''),
    NULLIF((SELECT (CASE WHEN j.source = 'git' AND j.repo_id IS NOT NULL
                         THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = j.repo_id)
                         ELSE j.owner_agency END)
              FROM jobs j WHERE j.uid = activity.job_uid), ''),
    NULLIF((SELECT (CASE WHEN w.source = 'git'
                         THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                         ELSE w.owner_agency END)
              FROM workflows w WHERE w.uid = activity.workflow_uid), ''),
    NULLIF(activity.agency_id, ''),
    'global')
  END;

UPDATE change_log SET agency_id = COALESCE(
    NULLIF((SELECT ru.owner_agency FROM runs ru WHERE ru.id = change_log.target), ''),
    NULLIF((SELECT wr.agency_id FROM workflow_runs wr WHERE wr.id = change_log.target), ''),
    CASE WHEN change_log.details LIKE 'workflow run %'
         THEN NULLIF((SELECT wr.agency_id FROM workflow_runs wr
                       WHERE wr.id = substr(change_log.details, length('workflow run ') + 1)), '') END,
    CASE WHEN change_log.target LIKE 'agency members: %'
         THEN (SELECT a.id FROM agencies a
                WHERE a.id = substr(change_log.target, length('agency members: ') + 1)) END,
    'global');

-- An agency's audit, read newest first (C1's filter).
CREATE INDEX idx_activity_agency_at ON activity(agency_id, at);
CREATE INDEX idx_change_log_agency_at ON change_log(agency_id, at);
CREATE INDEX idx_workflow_runs_agency_started ON workflow_runs(agency_id, started_at);
