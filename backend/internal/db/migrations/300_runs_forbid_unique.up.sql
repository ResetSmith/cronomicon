-- PP-L8: partial unique index on concurrency_key for active (queued/running) runs.
-- A duplicate INSERT for the same key while another run is active will fail with
-- UNIQUE constraint failed, closing the check-then-insert race window.
-- NULL concurrency_key rows (jobs with no Forbid policy) are excluded.
CREATE UNIQUE INDEX IF NOT EXISTS uq_runs_active_concurrency
ON runs (concurrency_key)
WHERE concurrency_key IS NOT NULL AND (status = 'queued' OR status = 'running');
