-- 680_runs_agencies_json — the per-run agency snapshot becomes a SET
-- (the agencies plan Phase 2, T2.3; decision AG-Q2b).
--
-- runs.agency (mig. 450) is a scalar NAME snapshot read by claimRun — the one
-- query every runner runs on every poll. Once a scope may belong to N agencies,
-- "the run's agency" is no longer a scalar and the claim predicate becomes a set
-- intersection inside that same atomic UPDATE … RETURNING.
--
-- Shape (AG-Q2b): a JSON array column, NOT a join table. This MIRRORS
-- runs.requires_json, which already does exactly this — a json_each intersection
-- inside the very same claim query — so it is a proven pattern in a proven place
-- rather than a new one. It also preserves mig. 450's deliberate denormalization:
-- a NAME snapshot with no FK, frozen at enqueue, so re-homing a scope cannot
-- silently retarget runs that are already queued. A run's injectable set and its
-- eligible runners are determined by the world as it was when it was authorized.
--
-- (A run_agencies join table stays in reserve as a pure MATERIALIZED INDEX if the
-- Phase-3 benchmark shows the third json_each scan exceeds its p99 budget. That
-- would change no semantics — see T3.3.)
--
-- NOTHING READS THIS COLUMN YET. Phase 2 dual-writes both this and the scalar
-- runs.agency; claimRun still matches on the scalar. That is what makes this
-- release rollback-safe: a reverted server reads the scalar and is entirely
-- correct. runs.agency is dropped in Phase 3's migration 690, once no reader
-- remains.
--
-- NULL vs '[]': the column is NOT NULL DEFAULT '[]' so every reader sees a
-- well-formed array and json_each never has to special-case NULL. An empty array
-- means the general pool — exactly what a NULL runs.agency means today.
ALTER TABLE runs ADD COLUMN agencies_json TEXT NOT NULL DEFAULT '[]';

-- Backfill from the scalar: NULL/'' → [] (general pool), else a single-element
-- array. json_quote gives correct escaping for an agency name containing a quote
-- or backslash, which naive string concatenation would corrupt into invalid JSON —
-- and invalid JSON here would make json_each throw inside the claim query, i.e.
-- stop dispatch fleet-wide.
UPDATE runs
SET agencies_json = json_array(agency)
WHERE agency IS NOT NULL AND agency <> '';
