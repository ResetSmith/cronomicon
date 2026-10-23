-- 1480_run_owner — a run records whose it is (2.4.4 S2, the agency switcher).
--
-- Until now a run's agency was read from agencies_json, the NAMES of its
-- scope's agencies when it was queued: a run with no scope was Global's,
-- whatever its job's agency, and a renamed agency lost its runs. The switcher
-- shows one agency's runs at a time, and the overview and the badges count
-- them per agency, so a run now carries its owner's id.
--
-- The owner is the run's job's owner when the run is queued (agencyid.JobSQL:
-- a Git job's repository's agency, else the job's owner_agency); for a run
-- whose job is gone or records none, its one snapshot agency; else Global's.
-- A trigger stamps it, so that every way a run is inserted is covered
-- (scheduler.InsertRun, the demo seed, a test's fixture). An insert that names
-- an owner keeps it.

ALTER TABLE runs ADD COLUMN owner_agency TEXT;

UPDATE runs SET owner_agency = COALESCE(
    NULLIF((SELECT CASE WHEN j.source = 'git' AND j.repo_id IS NOT NULL
                        THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = j.repo_id)
                        ELSE j.owner_agency END
              FROM jobs j WHERE j.uid = runs.job_uid), ''),
    (SELECT ag.id FROM agencies ag
      WHERE json_array_length(COALESCE(runs.agencies_json, '[]')) = 1
        AND ag.name = json_extract(runs.agencies_json, '$[0]')),
    'global');

CREATE TRIGGER runs_owner_stamped AFTER INSERT ON runs
WHEN NEW.owner_agency IS NULL
BEGIN
    UPDATE runs SET owner_agency = COALESCE(
        NULLIF((SELECT CASE WHEN j.source = 'git' AND j.repo_id IS NOT NULL
                            THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = j.repo_id)
                            ELSE j.owner_agency END
                  FROM jobs j WHERE j.uid = NEW.job_uid), ''),
        (SELECT ag.id FROM agencies ag
          WHERE json_array_length(COALESCE(NEW.agencies_json, '[]')) = 1
            AND ag.name = json_extract(NEW.agencies_json, '$[0]')),
        'global')
     WHERE rowid = NEW.rowid;
END;

-- The switcher's narrowing, the overview's 24-hour counts and the badges.
CREATE INDEX idx_runs_owner_started ON runs(owner_agency, started_at);
