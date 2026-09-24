-- Reverse 950. The same stash-and-restore applies in this direction: the DROP
-- below fires runs.workflow_run_id's ON DELETE SET NULL just as readily.
--
-- Nested workflow runs are DELETED rather than orphaned. Their trigger_kind
-- ('workflow') is about to become illegal, so leaving them would produce rows
-- the narrowed CHECK forbids — and a child run whose parent link no longer
-- exists is unattributable history, which is worse than no history.
DELETE FROM workflow_runs WHERE trigger_kind = 'workflow';

CREATE TABLE _950_run_wf_down AS
    SELECT id, workflow_run_id FROM runs WHERE workflow_run_id IS NOT NULL;

CREATE TABLE workflow_runs_old (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    triggered_by  TEXT NOT NULL,
    trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','webhook','reaction')),
    started_at    TEXT,
    completed_at  TEXT,
    created_at    TEXT NOT NULL,
    workflow_id            INTEGER,
    duration_ms            INTEGER,
    scope                  TEXT,
    schedule_name          TEXT,
    env_json               TEXT,
    workflow_source        TEXT,
    steps_snapshot         TEXT,
    steps_hash             TEXT,
    cancelled              INTEGER NOT NULL DEFAULT 0,
    cancelled_at           TEXT,
    suppressed_by_calendar TEXT,
    queued_reason          TEXT,
    reaction_depth         INTEGER NOT NULL DEFAULT 0,
    reacted_to_run_id      TEXT
);

INSERT INTO workflow_runs_old SELECT
    id, workflow_name, status, triggered_by, trigger_kind, started_at, completed_at, created_at,
    workflow_id, duration_ms, scope, schedule_name, env_json, workflow_source,
    steps_snapshot, steps_hash, cancelled, cancelled_at, suppressed_by_calendar,
    queued_reason, reaction_depth, reacted_to_run_id
FROM workflow_runs;

DROP TABLE workflow_runs;
ALTER TABLE workflow_runs_old RENAME TO workflow_runs;

UPDATE runs
   SET workflow_run_id = (SELECT workflow_run_id FROM _950_run_wf_down WHERE _950_run_wf_down.id = runs.id)
 WHERE id IN (SELECT id FROM _950_run_wf_down);
DROP TABLE _950_run_wf_down;

CREATE INDEX idx_workflow_runs_source_name
    ON workflow_runs (workflow_source, workflow_name);
CREATE INDEX idx_workflow_runs_suppressed_by_calendar
    ON workflow_runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
CREATE INDEX idx_workflow_runs_reacted_to
    ON workflow_runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;
