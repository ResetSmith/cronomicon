-- 240 Per-run step-graph snapshot (workflow-builder-update.md — WB-D1).
--
-- A workflow run's structure lived only in the live `workflows.steps` definition,
-- so once the definition was edited History could no longer show the graph as it
-- actually ran. These columns capture the step graph at trigger time — the same
-- per-run reproducibility pattern `runs.script_ref`/`content_hash` already give a
-- job run.
--
-- steps_snapshot: the JSON step graph as it ran, including the per-node trace IDs
-- the engine minted (assignNodeIDs), so a run's child runs map back to their graph
-- nodes for grouped parallel/branch rendering (WB-O3). steps_hash: a sha256 of the
-- snapshot for cheap "did the graph change?" comparisons. Both additive (ALTER ADD
-- COLUMN) — no rebuild. NULL ⇒ a pre-240 run (the serializer falls back to a flat
-- timeline).
ALTER TABLE workflow_runs ADD COLUMN steps_snapshot TEXT;
ALTER TABLE workflow_runs ADD COLUMN steps_hash TEXT;
