-- 150 Rollback script_ref indexes on jobs and runs.
DROP INDEX IF EXISTS idx_jobs_script_ref;
DROP INDEX IF EXISTS idx_runs_script_ref;
