-- Reverse 1070.
--
-- Losing `runner_tags` costs nothing: it is a projection of runners.tags, which
-- this migration does not touch, so a re-apply rebuilds it from the same source.
--
-- Losing the two `runner_tag` columns DOES destroy operator intent that exists
-- nowhere else — which runner a job was pinned to, and which one each historical
-- run was sent to. Git holds the job-level pin only for jobs that round-trip
-- through the YAML spec (RT-2, not yet landed at 1070), and nothing holds the
-- per-run one. More quietly: a rollback past this point does not merely forget
-- the pins, it makes every pinned job start running ANYWHERE eligible again,
-- because the claim predicate reverts with it. On a fleet where the pin encoded
-- VLAN reachability, that is jobs silently executing from the wrong network
-- segment rather than jobs visibly failing.
--
-- Index before table is not required (SQLite drops an index with its table) but
-- is written out so the reversal reads in the exact reverse order of the up.
DROP INDEX IF EXISTS idx_runner_tags_tag;
DROP TABLE IF EXISTS runner_tags;
ALTER TABLE runs DROP COLUMN runner_tag;
ALTER TABLE jobs DROP COLUMN runner_tag;
