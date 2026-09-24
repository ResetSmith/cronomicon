-- Reverse 1120.
--
-- LOSSY, deliberately and unavoidably: the rows in runner_placement_history are
-- snapshots of placement that no longer exists anywhere else -- the runners rows
-- they describe were deleted, which is why the table exists. Dropping it
-- destroys the only record of what those runners were bound to. There is no
-- re-derivation on a subsequent up, unlike 1100's derived stamp.
--
-- Take a backup before rolling back past this migration if any runner has been
-- deregistered since it was applied.
DROP INDEX IF EXISTS idx_runner_placement_history_at;
DROP INDEX IF EXISTS idx_runner_placement_history_name;
DROP TABLE IF EXISTS runner_placement_history;
ALTER TABLE runners DROP COLUMN last_client_ip;
