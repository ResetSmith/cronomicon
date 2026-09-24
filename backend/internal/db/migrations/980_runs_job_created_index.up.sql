-- 980 — the composite index the per-job "last run" lookups actually need.
--
-- FX-D1 made the jobs list carry TWO correlated subqueries per row: the newest
-- EXECUTED run and the newest SUPPRESSED one. Both filter (job_name,
-- job_source) and order by created_at. The only index that existed was
-- idx_runs_job_name(job_name), so each one narrowed to a job's rows and then
-- scanned and sorted them — on the deepest table in the product, twice per row,
-- in the query whose own header sells it as the one-round-trip design.
--
-- job_source is second because it is the lower-cardinality half (two values) and
-- because every one of these predicates supplies job_name; created_at last so
-- the ORDER BY is satisfied by the index rather than by a sort.
--
-- Deliberately NOT partial on status: the two lookups want opposite halves of
-- it (skipped, and everything else), so a partial index would serve one and
-- leave the other exactly as it was.
CREATE INDEX IF NOT EXISTS idx_runs_job_source_created
    ON runs (job_name, job_source, created_at);

-- The workflow twin, for the same two lookups on the workflows list.
CREATE INDEX IF NOT EXISTS idx_workflow_runs_name_source_created
    ON workflow_runs (workflow_name, workflow_source, created_at);
