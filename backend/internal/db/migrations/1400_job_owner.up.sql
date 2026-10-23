-- 1400_job_owner — a job built in the app belongs to one agency (2.4.0, §0a,
-- IN-5; Phase I2; the owner's answer to question 34).
--
-- A Git job's agency is its repository's. A job built in the app had none of
-- its own: whose it was came from its scope's name (the scope's agency when
-- one agency alone holds it, Global's otherwise). Once a scope's name is
-- unique within an agency only (I2), "which scope" depends on whose the job
-- is, and the job's agency cannot come from that name any more.
--
-- Each row is stamped with the answer it had until now, so nothing changes
-- whose a job is at this upgrade:
--   * a Git job: its repository's agency (Global's for a row with none), as
--     1360 stamped a Git workflow; the repository stays the answer for one
--     (agencyid.JobSQL), the column records it;
--   * a job built in the app: its scope's agency when one agency alone holds
--     the scope, Global's for no scope, a scope that is not there, or one that
--     several share.
-- From now on the composer writes it when a job is created. A row written
-- without one (sync's insert of a Git job, and anything else that does not
-- name an owner) is born with the same answer, by the trigger below, as a run
-- is born Global's (runs_born_global) and a scope its repository's
-- (scopes_born_global). An owner that is given is never replaced.

ALTER TABLE jobs ADD COLUMN owner_agency TEXT;

UPDATE jobs
   SET owner_agency = COALESCE((SELECT g.agency_id FROM git_repos g WHERE g.id = jobs.repo_id), 'global')
 WHERE source = 'git' AND repo_id IS NOT NULL;

-- A job built in the app, and a Git job that records no repository (none in
-- an installation since 1310 stamped Global's on every Git job; confined by
-- nothing, and its agency its scope's, as an in-app job's was).
UPDATE jobs
   SET owner_agency = COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
                                  FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
                                 WHERE sc.name = jobs.scope), 'global')
 WHERE owner_agency IS NULL;

CREATE TRIGGER jobs_born_owned AFTER INSERT ON jobs WHEN NEW.owner_agency IS NULL
BEGIN
    UPDATE jobs SET owner_agency = CASE WHEN NEW.source = 'git' AND NEW.repo_id IS NOT NULL
        THEN COALESCE((SELECT g.agency_id FROM git_repos g WHERE g.id = NEW.repo_id), 'global')
        ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
                         FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
                        WHERE sc.name = NEW.scope), 'global') END
     WHERE rowid = NEW.rowid;
END;

CREATE INDEX idx_jobs_owner_agency ON jobs (owner_agency);
