-- 030 B5 execution engine: add workflow_id FK + runner_id to workflow_runs/runs,
-- and add paused_jobs table for operator pause/resume.
-- The base tables are in 001_execution.up.sql; this migration is additive.

-- Store workflow_id (FK to workflows.name) on workflow_runs for cross-joining.
-- SQLite ALTER TABLE only supports ADD COLUMN, so we use a safe default.
ALTER TABLE workflow_runs ADD COLUMN workflow_id INTEGER;

-- Add duration_ms and scope to workflow_runs (not in the original 001 schema).
ALTER TABLE workflow_runs ADD COLUMN duration_ms INTEGER;
ALTER TABLE workflow_runs ADD COLUMN scope TEXT;

-- Paused jobs: operator pause/resume flag (per-instance override).
-- A paused job's cron fires are silently skipped.
CREATE TABLE IF NOT EXISTS paused_jobs (
    job_name   TEXT PRIMARY KEY,
    paused_by  TEXT NOT NULL,
    paused_at  TEXT NOT NULL
);

-- action_queue: operator kill signals delivered to runners on next poll.
-- B5 inserts kill signals; B4 reads + clears them via poll.
CREATE TABLE IF NOT EXISTS action_queue (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL,      -- trace id of the run to kill
    op         TEXT NOT NULL CHECK (op IN ('kill','drain')),
    created_at TEXT NOT NULL,
    consumed_at TEXT               -- set by B4 when the signal is delivered
);
CREATE INDEX IF NOT EXISTS idx_action_queue_run ON action_queue(run_id);

-- activity: add extra columns for workflow/job link fields (additive, safe to re-add)
ALTER TABLE activity ADD COLUMN job_id INTEGER;
ALTER TABLE activity ADD COLUMN workflow_id INTEGER;
ALTER TABLE activity ADD COLUMN action TEXT;
ALTER TABLE activity ADD COLUMN commit_sha TEXT;
ALTER TABLE activity ADD COLUMN repository TEXT;
ALTER TABLE activity ADD COLUMN branch TEXT;
ALTER TABLE activity ADD COLUMN duration_ms INTEGER;
ALTER TABLE activity ADD COLUMN killed_by TEXT;
