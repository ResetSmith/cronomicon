-- Reverse 170. A PK change cannot be undone by DROP COLUMN, so each rebuilt table
-- is rebuilt back to its pre-170 single-source schema. cronomicon-source rows cannot
-- exist in the old schema and are dropped on the way down (rollback to a schema
-- that predates dual-source).

-- ── jobs → PK (name) ────────────────────────────────────────────────────────
CREATE TABLE jobs_old (
    name               TEXT PRIMARY KEY,
    run_type           TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl')),
    description        TEXT,
    scope              TEXT,
    target_host        TEXT,
    schedule           TEXT,
    tags               TEXT NOT NULL DEFAULT '[]',
    enabled            INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    timeout_seconds    INTEGER,
    retries            INTEGER NOT NULL DEFAULT 0,
    requestable        INTEGER NOT NULL DEFAULT 0 CHECK (requestable IN (0,1)),
    concurrency_policy TEXT NOT NULL DEFAULT 'Allow' CHECK (concurrency_policy IN ('Allow','Forbid','Replace')),
    concurrency_key    TEXT,
    sensitive_logging  INTEGER NOT NULL DEFAULT 0 CHECK (sensitive_logging IN (0,1)),
    source_path        TEXT,
    synced_at          TEXT NOT NULL DEFAULT '',
    command            TEXT,
    script             TEXT,
    script_path        TEXT,
    executor           TEXT CHECK (executor IN ('runner','ssh')),
    script_ref         TEXT,
    content_hash       TEXT
);
INSERT INTO jobs_old (name, run_type, description, scope, target_host, schedule, tags,
                      enabled, timeout_seconds, retries, requestable, concurrency_policy,
                      concurrency_key, sensitive_logging, source_path, synced_at,
                      command, script, script_path, executor, script_ref, content_hash)
    SELECT name, run_type, description, scope, target_host, schedule, tags,
           enabled, timeout_seconds, retries, requestable, concurrency_policy,
           concurrency_key, sensitive_logging, source_path, COALESCE(synced_at,''),
           command, script, script_path, executor, script_ref, content_hash
    FROM jobs WHERE source = 'git';
DROP TABLE jobs;
ALTER TABLE jobs_old RENAME TO jobs;

-- ── workflows → PK (name) ───────────────────────────────────────────────────
CREATE TABLE workflows_old (
    name        TEXT PRIMARY KEY,
    description TEXT,
    steps       TEXT NOT NULL DEFAULT '[]',
    schedule    TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    source_path TEXT,
    synced_at   TEXT NOT NULL DEFAULT ''
);
INSERT INTO workflows_old (name, description, steps, schedule, enabled, source_path, synced_at)
    SELECT name, description, steps, schedule, enabled, source_path, COALESCE(synced_at,'')
    FROM workflows WHERE source = 'git';
DROP TABLE workflows;
ALTER TABLE workflows_old RENAME TO workflows;

-- ── paused_jobs → PK (job_name); recollapse workflows to the __wf__ sentinel ─
CREATE TABLE paused_jobs_old (
    job_name  TEXT PRIMARY KEY,
    paused_by TEXT NOT NULL,
    paused_at TEXT NOT NULL
);
INSERT INTO paused_jobs_old (job_name, paused_by, paused_at)
    SELECT CASE WHEN owner_kind = 'workflow' THEN '__wf__' || name ELSE name END,
           paused_by, paused_at
    FROM paused_jobs WHERE source = 'git';
DROP TABLE paused_jobs;
ALTER TABLE paused_jobs_old RENAME TO paused_jobs;

-- ── definition_schedules → PK (owner_kind, owner_name, name) ────────────────
CREATE TABLE definition_schedules_old (
    owner_kind  TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name  TEXT NOT NULL,
    name        TEXT NOT NULL,
    cron        TEXT NOT NULL,
    env         TEXT,
    position    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_name, name)
);
INSERT INTO definition_schedules_old (owner_kind, owner_name, name, cron, env, position)
    SELECT owner_kind, owner_name, name, cron, env, position
    FROM definition_schedules WHERE owner_source = 'git';
DROP TABLE definition_schedules;
ALTER TABLE definition_schedules_old RENAME TO definition_schedules;

-- ── drop additive run columns ───────────────────────────────────────────────
ALTER TABLE runs DROP COLUMN job_source;
ALTER TABLE workflow_runs DROP COLUMN workflow_source;

-- ── recreate the original (non-source) indexes + triggers ───────────────────
CREATE INDEX idx_jobs_schedule ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_def_schedules_owner ON definition_schedules(owner_kind, owner_name);

CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules WHERE owner_kind = 'job' AND owner_name = OLD.name;
END;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules WHERE owner_kind = 'workflow' AND owner_name = OLD.name;
END;
