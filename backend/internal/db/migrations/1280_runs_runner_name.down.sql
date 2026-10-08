-- Reverse 1280.
--
-- `runner_name` is a derived stamp. A down/up cycle re-derives it for every
-- run whose runner is still registered; what it cannot recover is the name on
-- a run whose runner was deregistered after the run was stamped.
ALTER TABLE runs DROP COLUMN runner_name;
