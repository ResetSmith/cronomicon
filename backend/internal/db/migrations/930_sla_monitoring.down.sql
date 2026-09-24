-- Reverse 930 completely: the indexes AND the columns.
--
-- SQLite has supported ALTER TABLE ... DROP COLUMN since 3.35 and the bundled
-- driver is well past that, so this down is a real inverse rather than a
-- partial one — which matters because a partial down leaves the columns behind
-- and makes RE-APPLYING the migration fail on "duplicate column name". An
-- operator who rolls back and rolls forward again must not be wedged.
--
-- The index on runs(started_at) has to go first: SQLite refuses to drop a column
-- an index references. It does not reference sla_warned_at, but dropping it here
-- keeps the order obviously safe.
DROP INDEX IF EXISTS idx_runs_running_started;
DROP INDEX IF EXISTS idx_runs_job_created;

ALTER TABLE runs DROP COLUMN sla_warned_at;
ALTER TABLE jobs DROP COLUMN must_finish_by;
ALTER TABLE jobs DROP COLUMN warn_after_seconds;
