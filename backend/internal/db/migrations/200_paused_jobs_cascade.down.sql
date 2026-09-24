-- Reverse 200. Drop the paused_jobs cascade triggers. The one-time orphan
-- cleanup in the up migration is not reversible (the rows were stale orphans).
DROP TRIGGER IF EXISTS trg_paused_jobs_job_delete;
DROP TRIGGER IF EXISTS trg_paused_jobs_workflow_delete;
