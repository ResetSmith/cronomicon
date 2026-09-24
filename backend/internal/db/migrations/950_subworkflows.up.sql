-- 950_subworkflows — a workflow may be a step of another workflow (SW,
-- the prod-features plan §6).
--
-- ⚠️ THIS REVERSES A RECORDED DECISION. Migration 890's header says, of the two
-- trigger_kind vocabularies:
--
--     "a workflow is never triggered by a workflow — there is no nesting"
--
-- That was true and deliberate. SW makes it false, so the value is added here
-- and the old comment is replaced rather than left to mislead.
--
-- ⚠️ AND IT IS THE DANGEROUS REBUILD. `runs.workflow_run_id` is a real foreign
-- key into this table with ON DELETE SET NULL, and the pool opens with
-- _foreign_keys=on, so the DROP TABLE below fires that action and NULLs the
-- parent link on EVERY child run ever recorded. History would silently
-- de-nest itself across the whole install.
--
-- So the link is stashed before the drop and restored after, exactly as 830
-- established and 890 documented. This is the third time this recipe has been
-- needed; read 890's header before writing the fourth.

-- ── Stash the inbound FK's values ───────────────────────────────────────────
CREATE TABLE _950_run_wf AS
    SELECT id, workflow_run_id FROM runs WHERE workflow_run_id IS NOT NULL;

CREATE TABLE workflow_runs_new (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    triggered_by  TEXT NOT NULL,
    -- 'workflow' is now legal: a parent workflow step triggers this run (SW).
    trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','webhook','reaction','workflow')),
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
    reacted_to_run_id      TEXT,
    -- SW — the parent link. Self-referential, ON DELETE SET NULL for the same
    -- reason runs.workflow_run_id is: retention pruning a parent must orphan the
    -- child's provenance, never delete the child's history.
    parent_workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE SET NULL,
    -- Which STEP of the parent produced this run. The parent id alone cannot say
    -- that, and a parent with two workflow steps needs to render each in its own
    -- place on the canvas.
    parent_node_id         TEXT,
    -- SW nesting depth, distinct from reaction_depth. A sub-workflow inside a
    -- reaction chain consumes both budgets independently — they count different
    -- things and collapsing them would make either ceiling unpredictable.
    workflow_depth         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO workflow_runs_new (
    id, workflow_name, status, triggered_by, trigger_kind, started_at, completed_at, created_at,
    workflow_id, duration_ms, scope, schedule_name, env_json, workflow_source,
    steps_snapshot, steps_hash, cancelled, cancelled_at, suppressed_by_calendar,
    queued_reason, reaction_depth, reacted_to_run_id)
SELECT
    id, workflow_name, status, triggered_by, trigger_kind, started_at, completed_at, created_at,
    workflow_id, duration_ms, scope, schedule_name, env_json, workflow_source,
    steps_snapshot, steps_hash, cancelled, cancelled_at, suppressed_by_calendar,
    queued_reason, reaction_depth, reacted_to_run_id
FROM workflow_runs;

DROP TABLE workflow_runs;
ALTER TABLE workflow_runs_new RENAME TO workflow_runs;

-- ── Restore what the cascade just destroyed ─────────────────────────────────
UPDATE runs
   SET workflow_run_id = (SELECT workflow_run_id FROM _950_run_wf WHERE _950_run_wf.id = runs.id)
 WHERE id IN (SELECT id FROM _950_run_wf);
DROP TABLE _950_run_wf;

-- Indexes (3 existing + 1 new).
CREATE INDEX idx_workflow_runs_source_name
    ON workflow_runs (workflow_source, workflow_name);
CREATE INDEX idx_workflow_runs_suppressed_by_calendar
    ON workflow_runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
CREATE INDEX idx_workflow_runs_reacted_to
    ON workflow_runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;
-- "show me this parent's children", the drill-down History needs.
CREATE INDEX idx_workflow_runs_parent
    ON workflow_runs(parent_workflow_run_id) WHERE parent_workflow_run_id IS NOT NULL;
