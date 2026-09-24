-- Reverse 1000.
--
-- LOSSY by design: the uids are destroyed, and a re-up mints fresh ones. That is
-- acceptable ONLY while nothing references a uid — true for the whole of AF-4a,
-- which is the window this down-migration exists in. The moment AF-4b moves a
-- referencing edge onto uids, rolling back through this migration would orphan
-- that edge, and 4b's own migration must replace this file's guarantees.
DROP INDEX IF EXISTS idx_jobs_uid;
DROP INDEX IF EXISTS idx_workflows_uid;
DROP INDEX IF EXISTS idx_schedules_uid;
ALTER TABLE jobs      DROP COLUMN uid;
ALTER TABLE workflows DROP COLUMN uid;
ALTER TABLE schedules DROP COLUMN uid;
