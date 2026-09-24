-- 090 multi-schedule support: named cron entries per job/workflow definition.
--
-- A job or workflow may carry multiple named schedules (each its own cron +
-- optional env). The legacy single `schedule` column on jobs/workflows is kept
-- as a denormalized display field (the lowest-position entry's cron, or 'Manual')
-- so existing readers (deriveJobStatus, Dashboard isScheduled, the builder
-- job-picker, the frozen Job.schedule spec field) keep working. GitLab remains
-- the source of truth; sync rewrites these rows (delete-by-owner + insert).

CREATE TABLE definition_schedules (
    owner_kind  TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name  TEXT NOT NULL,
    name        TEXT NOT NULL,            -- entry name; 'default' for the legacy single schedule
    cron        TEXT NOT NULL,
    env         TEXT,                     -- JSON object map[string]string; NULL = none
    position    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_name, name)
);
CREATE INDEX idx_def_schedules_owner ON definition_schedules(owner_kind, owner_name);

-- Backfill from the legacy single-schedule columns ('Manual'/empty = no entry).
INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position)
    SELECT 'job', name, 'default', schedule, 0 FROM jobs
    WHERE schedule IS NOT NULL AND schedule != '' AND schedule != 'Manual';
INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position)
    SELECT 'workflow', name, 'default', schedule, 0 FROM workflows
    WHERE schedule IS NOT NULL AND schedule != '' AND schedule != 'Manual';

-- Cascade-delete: SQLite has no polymorphic FK, so triggers keep the child table
-- consistent if a parent definition row is ever deleted. (No delete path exists
-- today — sync only upserts — this is forward-looking insurance.)
CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules WHERE owner_kind = 'job' AND owner_name = OLD.name;
END;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules WHERE owner_kind = 'workflow' AND owner_name = OLD.name;
END;

-- Run traceability: which schedule entry fired, and the env snapshot it carried.
ALTER TABLE runs ADD COLUMN schedule_name TEXT;
ALTER TABLE runs ADD COLUMN env_json TEXT;
ALTER TABLE workflow_runs ADD COLUMN schedule_name TEXT;
ALTER TABLE workflow_runs ADD COLUMN env_json TEXT;
