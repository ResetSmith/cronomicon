-- 1190_drop_runner_pin — retire the runner-tag pin (the scope-bound-runners
-- plan, SB-3).
--
-- 1070 let a job say "only a runner carrying this tag may claim my runs" and
-- gave the claim query a `runner_tags` projection of runners.tags to match it
-- against. That made a runner's tags the one place in the product where a tag
-- decided anything, and recorded a fact about where a scope's HOSTS are — which
-- runners can reach them — on every job that happened to target that scope.
-- 1180 moved the fact to the scope (scope_runners) and converted the pins that
-- could be converted; every reader has since moved. This removes what is left:
--
--   jobs.runner_tag   the declared pin. Git's `runner_tag:` key is still
--                     recognised, to warn about a line left in a repository,
--                     but it is no longer stored or read for dispatch.
--   runner_tags       the projection. runners.tags stays, as plain labels.
--
-- runs.runner_tag is deliberately KEPT, and no longer written. It is history:
-- "this run was pinned to vlan-dmz" stays true of a run from last month, and
-- History still shows it. Dropping it would cost a `runs` rebuild — the
-- operation migration 890 documents the hazards of — to delete information.
--
-- Pins that 1180 could not convert are in retired_runner_pins. Nothing else
-- records them once this column is gone, which is why that table exists.
DROP INDEX IF EXISTS idx_runner_tags_tag;
DROP TABLE IF EXISTS runner_tags;

ALTER TABLE jobs DROP COLUMN runner_tag;
