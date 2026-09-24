-- Reverse 1010.
--
-- Safe (not lossy in any way that matters): the dropped columns are derived
-- stamps whose truth still lives in the name columns plus the definitions'
-- own uid columns (migration 1000) — a re-up re-derives them identically for
-- every row whose definition still exists. Only rows written between up and
-- down whose definition was ALSO deleted in that window lose the stamp, which
-- is the same NULL those rows would have carried anyway.
DROP INDEX IF EXISTS idx_runs_job_uid_created;
DROP INDEX IF EXISTS idx_workflow_runs_uid_created;
ALTER TABLE runs          DROP COLUMN job_uid;
ALTER TABLE workflow_runs DROP COLUMN workflow_uid;
ALTER TABLE activity      DROP COLUMN job_uid;
ALTER TABLE activity      DROP COLUMN workflow_uid;
