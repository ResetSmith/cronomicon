-- Reverse 680. runs.agency (mig. 450) is still dual-written and still the column
-- claimRun matches on through Phase 2, so dropping the JSON snapshot loses nothing
-- a reverted server needs — which is the whole point of the dual-write release.
--
-- SQLite has supported ALTER TABLE … DROP COLUMN since 3.35; the column carries no
-- index and no constraint, so no table rebuild is required (unlike the run_type
-- CHECK widening in migration 490).
ALTER TABLE runs DROP COLUMN agencies_json;
