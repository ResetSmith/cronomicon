-- 150 Index script_ref columns on jobs and runs.
-- Useful for fast used-by reverse lookups and executions history filters.
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_runs_script_ref ON runs(script_ref) WHERE script_ref IS NOT NULL;
