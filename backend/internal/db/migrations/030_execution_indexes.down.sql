-- 030 down: remove additive columns and tables.
-- SQLite doesn't support DROP COLUMN in all versions, so we recreate tables.
DROP TABLE IF EXISTS action_queue;
DROP TABLE IF EXISTS paused_jobs;
-- Note: ALTER TABLE DROP COLUMN is supported in SQLite 3.35+
-- The columns added to workflow_runs and runs and activity are left in place
-- on down since SQLite 3.35+ is required; the data is harmless.
