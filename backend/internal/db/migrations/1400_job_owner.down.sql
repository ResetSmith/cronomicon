-- 1400_job_owner (down) — jobs have no owner again; a job built in the app is
-- its scope's agency's, worked out from the scope's name.

DROP TRIGGER IF EXISTS jobs_born_owned;
DROP INDEX IF EXISTS idx_jobs_owner_agency;
ALTER TABLE jobs DROP COLUMN owner_agency;
