-- 270 Real run→definition linkage (workflow-builder-update.md — WB-D2).
--
-- "Runs for workflow X" historically filtered on workflow_runs.workflow_id — a
-- bare, unindexed INTEGER (the workflows rowid at trigger time) with no stable
-- relationship to the workflows PK (source, name). A rowid can be reused after a
-- delete+recreate, stranding/mis-attributing runs. The stable identity is
-- (workflow_source, workflow_name), which the run already records; this composite
-- index makes filtering by it cheap so the serializers can switch to it.
CREATE INDEX IF NOT EXISTS idx_workflow_runs_source_name
    ON workflow_runs (workflow_source, workflow_name);
