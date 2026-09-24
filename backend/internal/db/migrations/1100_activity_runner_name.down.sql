-- Reverse 1100.
--
-- Not lossy in the sense 1010's down documents: `runner_name` is a DERIVED
-- stamp, and re-running the up re-derives it identically for every row whose
-- evidence still exists -- the run's `runner_id` join, or the `runner:<name>`
-- prefix in `target`. What a down/up cycle cannot recover is a row whose
-- runner was deregistered in the window, and that row would have carried NULL
-- from the backfill anyway.
DROP INDEX IF EXISTS idx_activity_actor_at;
DROP INDEX IF EXISTS idx_activity_runner_at;
ALTER TABLE activity DROP COLUMN runner_name;
