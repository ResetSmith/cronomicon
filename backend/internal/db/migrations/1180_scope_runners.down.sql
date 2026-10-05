-- Reverse 1180 — drop the scope↔runner bindings and the retired-pin notices.
-- LOSSY by nature: both exist nowhere else. The pins the up migration converted
-- are still on jobs.runner_tag at this version, so dispatch falls back to them.
DROP INDEX IF EXISTS idx_retired_runner_pins_scope;
DROP TABLE IF EXISTS retired_runner_pins;
DROP INDEX IF EXISTS idx_scope_runners_runner;
DROP TABLE IF EXISTS scope_runners;
