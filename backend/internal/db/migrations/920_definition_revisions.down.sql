-- Reverse 920 completely.
--
-- definition_revisions is destroyed outright — the snapshot history has no other
-- home, and a reversible record of what was deleted is not a record.
--
-- The soft-delete columns are genuinely dropped rather than left behind. SQLite
-- has supported ALTER TABLE ... DROP COLUMN since 3.35 and the bundled driver is
-- well past that, so leaving them would buy nothing and would make RE-APPLYING
-- this migration fail on "duplicate column name" — wedging any operator who
-- rolls back and then forward again.
--
-- Note what this means for data: rolling back RESURRECTS every binned
-- definition, because the stamp that hid them is gone. That is the correct
-- direction to fail — a rollback that silently destroyed recoverable
-- definitions would be far worse — but it is not a no-op, and anything in the
-- recycle bin at rollback time comes back live.
--
-- The partial indexes must be dropped before their columns; SQLite refuses to
-- drop a column an index references.
DROP INDEX IF EXISTS idx_definition_revisions_age;
DROP INDEX IF EXISTS uq_definition_revisions;
DROP TABLE IF EXISTS definition_revisions;

DROP INDEX IF EXISTS idx_jobs_deleted;
DROP INDEX IF EXISTS idx_workflows_deleted;
DROP INDEX IF EXISTS idx_schedules_deleted;

ALTER TABLE jobs DROP COLUMN deleted_by;
ALTER TABLE jobs DROP COLUMN deleted_at;
ALTER TABLE workflows DROP COLUMN deleted_by;
ALTER TABLE workflows DROP COLUMN deleted_at;
ALTER TABLE schedules DROP COLUMN deleted_by;
ALTER TABLE schedules DROP COLUMN deleted_at;
