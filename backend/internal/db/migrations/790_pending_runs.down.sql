DROP TRIGGER IF EXISTS trg_pending_runs_workflow_delete;
DROP TRIGGER IF EXISTS trg_pending_runs_job_delete;
DROP INDEX IF EXISTS idx_pending_runs_due;
DROP TABLE IF EXISTS pending_runs;
