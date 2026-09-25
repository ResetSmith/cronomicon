-- 180 Inter-job env var passing (cronomicon-v20.md — A12, Phase 5).
--
-- A workflow step may emit named outputs (KEY=value) that downstream steps consume
-- as injected env (per-step bus + explicit {fromStep,fromOutput}, decision Q-F).
-- The executor parses output markers from the run's stdout and persists them here;
-- the engine reads this column to populate JobResult.Outputs (activating the
-- output_match branch condition) and to resolve a downstream step's declared inputs.
-- Additive (ALTER ADD COLUMN) — no rebuild. NULL ⇒ no captured outputs.
ALTER TABLE runs ADD COLUMN outputs_json TEXT;
