-- 1280_runs_runner_name — a run keeps the name of the runner that took it
-- (2.3.0, LR-50).
--
-- History showed an Executor on every run. Every run is the runner executor's
-- since 2.3.0, so that column says the same thing on every new row, and the
-- useful fact is WHICH runner took the run: an agent, or the server as the
-- local runner. `runs.runner_id` already says so, but it is ON DELETE SET NULL
-- and a runner row does not live long: the reaper's RunnerDeregisterAfter
-- sweep deletes it, and a re-enrolled agent has a new id. A name resolved by a
-- join would go blank for every run of a runner that has been retired or
-- replaced, which is exactly when someone asks what it ran.
--
-- So this is a SNAPSHOT, for the reasons 1100 gives for `activity.runner_name`,
-- 450 for `runs.agency` and 710 for `runs.entity_code`: history keeps its own
-- copy of the catalog value it referred to. The claim stamps it (runner.Claim,
-- the one statement that sets runner_id); nothing else writes it, and a runner
-- renamed later does not rewrite the runs it took under its old name.
ALTER TABLE runs ADD COLUMN runner_name TEXT;

-- Backfill where the join is still exact: a run whose runner is registered
-- today. A run whose runner is gone has no name to recover and stays NULL, as
-- does a run nobody claimed and a run the in-app SSH executor ran before 2.3.0
-- (`executor = 'ssh'`), which had no runner.
UPDATE runs
   SET runner_name = (SELECT rn.name FROM runners rn WHERE rn.id = runs.runner_id)
 WHERE runner_id IS NOT NULL;
