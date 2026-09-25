-- Reverse 940.
--
-- The jobs rebuild is repeated in the other direction because the CHECK cannot
-- be ALTERed either way. Queue-policy jobs coerce to Forbid rather than Allow:
-- Queue means "one at a time", and Forbid is the surviving policy that still
-- means that. Coercing to Allow would silently let a job that was configured
-- never to overlap start overlapping — the one outcome a rollback must not
-- produce.
--
-- Any pending rows parked behind a gate are deleted: their fire condition
-- ("the gate cleared") no longer exists once the columns are gone, and leaving
-- them would let the promoter fire a run whose queue semantics are no longer
-- implemented.
UPDATE jobs SET concurrency_policy = 'Forbid' WHERE concurrency_policy = 'Queue';
DELETE FROM pending_runs WHERE gate_kind = 'concurrency';

DROP INDEX IF EXISTS idx_pending_runs_queue;
ALTER TABLE pending_runs DROP COLUMN gate_kind;
ALTER TABLE pending_runs DROP COLUMN concurrency_key;

DROP INDEX IF EXISTS idx_runs_claimable;
ALTER TABLE runs DROP COLUMN priority;
CREATE INDEX idx_runs_claimable ON runs (status, executor, created_at);

CREATE TABLE jobs_old (
    name               TEXT NOT NULL,
    source             TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    run_type           TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl','python')),
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
    source_path        TEXT,
    synced_at          TEXT,
    command            TEXT,
    script             TEXT,
    script_path        TEXT,
    executor           TEXT CHECK (executor IN ('runner','ssh')),
    script_ref         TEXT,
    content_hash       TEXT,
    created_by         TEXT,
    created_at         TEXT,
    last_modified_by   TEXT,
    last_modified_at   TEXT,
    env_json           TEXT,
    backoff_seconds    INTEGER NOT NULL DEFAULT 0,
    continue_on_error  INTEGER NOT NULL DEFAULT 0,
    prompts_json       TEXT NOT NULL DEFAULT '[]',
    env_passthrough    TEXT NOT NULL DEFAULT '[]',
    project_root       TEXT,
    requires_json      TEXT NOT NULL DEFAULT '[]',
    prompt_enforcement TEXT NOT NULL DEFAULT 'warn' CHECK (prompt_enforcement IN ('warn','block')),
    ssh_user           TEXT,
    ssh_credential     TEXT,
    become_password_secret TEXT,
    deleted_at         TEXT,
    deleted_by         TEXT,
    warn_after_seconds INTEGER,
    must_finish_by     TEXT,
    PRIMARY KEY (source, name)
);

INSERT INTO jobs_old SELECT
    name, source, run_type, description, scope, target_host, schedule, tags, enabled,
    timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
    source_path, synced_at, command, script, script_path, executor, script_ref, content_hash,
    created_by, created_at, last_modified_by, last_modified_at, env_json,
    backoff_seconds, continue_on_error, prompts_json, env_passthrough, project_root,
    requires_json, prompt_enforcement, ssh_user, ssh_credential, become_password_secret,
    deleted_at, deleted_by, warn_after_seconds, must_finish_by
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_old RENAME TO jobs;

CREATE INDEX idx_jobs_schedule ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_jobs_deleted ON jobs(deleted_at) WHERE deleted_at IS NOT NULL;

CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'job' AND owner_name = OLD.name AND owner_source = OLD.source;
END;

CREATE TRIGGER trg_paused_jobs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'job' AND name = OLD.name AND source = OLD.source;
END;

CREATE TRIGGER trg_pending_runs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM pending_runs WHERE kind = 'job' AND source = OLD.source AND name = OLD.name;
END;

CREATE TRIGGER reference_bindings_job_cleanup AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

CREATE TRIGGER trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;
