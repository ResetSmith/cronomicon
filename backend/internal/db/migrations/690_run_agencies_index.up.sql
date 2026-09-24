-- 690_run_agencies_index — the materialized lookup index for the claim query
-- (the agencies plan T3.3, the documented AG-Q2b fallback).
--
-- WHY THIS EXISTS. Phase 3 replaces claimRun's scalar `agency = ?` match with a
-- set intersection over runs.agencies_json. The plan accepted that shape only if a
-- benchmark showed the regression within 15%. It did not:
--
--   10k queued runs × 50 runners × 20 agencies, 300 claims × 5 runs
--     scalar `agency IN (…)`                    ~4.44 ms/claim   (baseline)
--     json_each(agencies_json) ∩ runner_agencies ~20.0 ms/claim   (+350%)
--
-- json_each cannot be indexed, so the intersection re-parses the JSON array of
-- EVERY candidate row on EVERY poll — and the candidate set is the whole queue.
-- The plan's stated fallback is exactly this table: a materialized index
-- maintained alongside agencies_json, "semantics unchanged, snapshot discipline
-- unchanged, purely a lookup structure. That is option (c) without its design
-- cost." The benchmark stays checked in (poll_claim_bench_test.go) with all three
-- predicates so the numbers behind this decision remain reproducible.
--
-- WHAT IS AUTHORITATIVE. runs.agencies_json remains THE snapshot — the thing
-- readers, the API and reproducibility depend on. This table is derived from it and
-- written in the same statement. If they ever disagree, agencies_json is right and
-- this is a stale index; SyncRunAgencies re-derives it.
--
-- Snapshot discipline (mig. 450) is preserved: agency NAMES, no FK to agencies, so
-- deleting or renaming an agency cannot retroactively rewrite what a run required.
-- The FK to runs(id) with ON DELETE CASCADE is deliberate and is the only one — it
-- is what keeps run-retention purges from leaving orphaned index rows.
CREATE TABLE run_agencies (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  agency TEXT NOT NULL,
  PRIMARY KEY (run_id, agency)
);

-- The claim query probes run_id (correlated to the candidate row) — that is the
-- composite PK's leading column, so it is an index seek. The agency-side index
-- serves the reverse question ("which queued runs target DSS?") that the stuck-run
-- signal and the Phase-4 agency detail ask.
CREATE INDEX idx_run_agencies_agency ON run_agencies (agency);

-- Backfill from the authoritative snapshot. json_each over the whole runs table
-- ONCE at migration time is fine; doing it per poll is exactly what this replaces.
INSERT OR IGNORE INTO run_agencies (run_id, agency)
SELECT r.id, je.value
FROM runs r, json_each(COALESCE(r.agencies_json, '[]')) je
WHERE je.value IS NOT NULL AND je.value <> '';

-- ── The claim query's scan index ────────────────────────────────────────────
--
-- Measuring the predicates exposed something with nothing to do with agencies:
-- claimRun had NO usable index at all. `EXPLAIN QUERY PLAN` showed `SCAN runs` plus
-- `USE TEMP B-TREE FOR ORDER BY` — it examined every run in the table and sorted
-- them, on every poll, from every runner. That full scan is why the pre-Phase-3
-- baseline itself costs ~4.4 ms on a 10k-deep queue, and it is what made every
-- candidate agency predicate look catastrophic: each was multiplying a scan that
-- should never have been happening.
--
-- This composite index covers the claim query exactly: the two equality columns
-- first, then the ORDER BY column. SQLite can now seek to the queued runner runs and
-- walk them oldest-first, stopping at the first match — no scan, no temp B-tree.
--
-- Deliberately NOT a partial index (`WHERE status='queued' AND executor='runner'`):
-- that was tried first and the planner declined to use it here, falling back to the
-- full scan. The composite form is chosen, verified by EXPLAIN QUERY PLAN in
-- poll_claim_bench_test.go, which asserts the plan rather than trusting it.
CREATE INDEX idx_runs_claimable ON runs (status, executor, created_at);
