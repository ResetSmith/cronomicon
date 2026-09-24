-- 450_runs_agency — per-run agency snapshot for hard-isolation dispatch
-- (agency-support.md M3, §3.4). A denormalized NAME snapshot (no FK), mirroring
-- runs.scope and the per-run reproducibility-snapshot discipline: the agency a run
-- requires is frozen from its effective scope at enqueue, immutable, and matched
-- against runner membership in claimRun. NULL = general pool / unscoped run.
ALTER TABLE runs ADD COLUMN agency TEXT;
