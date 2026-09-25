-- Reverse 1050 — restore PRIMARY KEY (source, name) on all three tables.
--
-- REFUSES, LOUDLY, if duplicate names exist. If two cronomicon jobs named
-- 'backup' were created after the up-migration, this INSERT hits the restored
-- PK and the migration fails dirty rather than silently discarding one
-- department's job. That is the correct behaviour: rolling back past the
-- naming relaxation with duplicate names in place is a decision an operator
-- must make by renaming, not one a migration may make by deletion.

-- The scripts-table trigger (820) references reference_bindings; it must not
-- exist while that table is mid-rebuild (ALTER RENAME re-parses every trigger).
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;

CREATE TABLE jobs_old (
    uid                TEXT,
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
    concurrency_policy TEXT NOT NULL DEFAULT 'Allow' CHECK (concurrency_policy IN ('Allow','Forbid','Queue')),
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
    watch_json         TEXT,
    PRIMARY KEY (source, name)
);

INSERT INTO jobs_old (uid, name, source, run_type, description, scope, target_host, schedule, tags,
    enabled, timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
    source_path, synced_at, command, script, script_path, executor, script_ref,
    content_hash, created_by, created_at, last_modified_by, last_modified_at,
    env_json, backoff_seconds, continue_on_error, prompts_json, env_passthrough,
    project_root, requires_json, prompt_enforcement, ssh_user, ssh_credential,
    become_password_secret, deleted_at, deleted_by, warn_after_seconds,
    must_finish_by, watch_json)
SELECT uid, name, source, run_type, description, scope, target_host, schedule, tags,
    enabled, timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
    source_path, synced_at, command, script, script_path, executor, script_ref,
    content_hash, created_by, created_at, last_modified_by, last_modified_at,
    env_json, backoff_seconds, continue_on_error, prompts_json, env_passthrough,
    project_root, requires_json, prompt_enforcement, ssh_user, ssh_credential,
    become_password_secret, deleted_at, deleted_by, warn_after_seconds,
    must_finish_by, watch_json
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_old RENAME TO jobs;

CREATE INDEX idx_jobs_schedule   ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_jobs_deleted    ON jobs(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE UNIQUE INDEX idx_jobs_uid ON jobs(uid);


CREATE TABLE workflows_old (
    uid              TEXT,
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    description      TEXT,
    steps            TEXT NOT NULL DEFAULT '[]',
    schedule         TEXT,
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    source_path      TEXT,
    synced_at        TEXT,
    created_by       TEXT,
    created_at       TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    layout_json      TEXT,
    deleted_at       TEXT,
    deleted_by       TEXT,
    PRIMARY KEY (source, name)
);

INSERT INTO workflows_old (uid, name, source, description, steps, schedule, enabled, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, layout_json, deleted_at, deleted_by)
SELECT uid, name, source, description, steps, schedule, enabled, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, layout_json, deleted_at, deleted_by
FROM workflows;

DROP TABLE workflows;
ALTER TABLE workflows_old RENAME TO workflows;

CREATE INDEX idx_workflows_deleted ON workflows(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE UNIQUE INDEX idx_workflows_uid ON workflows(uid);


CREATE TABLE schedules_old (
    uid              TEXT,
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    description      TEXT,
    cron             TEXT NOT NULL,
    env              TEXT,
    content_hash     TEXT NOT NULL,
    source_path      TEXT,
    synced_at        TEXT,
    created_by       TEXT,
    created_at       TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    start_at         TEXT,
    end_at           TEXT,
    interval         TEXT,
    skip_calendars   TEXT,
    only_calendars   TEXT,
    deleted_at       TEXT,
    deleted_by       TEXT,
    PRIMARY KEY (source, name)
);

INSERT INTO schedules_old (uid, name, source, description, cron, env, content_hash, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, start_at, end_at, interval, skip_calendars, only_calendars,
    deleted_at, deleted_by)
SELECT uid, name, source, description, cron, env, content_hash, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, start_at, end_at, interval, skip_calendars, only_calendars,
    deleted_at, deleted_by
FROM schedules;

DROP TABLE schedules;
ALTER TABLE schedules_old RENAME TO schedules;

CREATE INDEX idx_schedules_deleted ON schedules(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE UNIQUE INDEX idx_schedules_uid ON schedules(uid);

DROP INDEX IF EXISTS uq_file_watch_sightings;
CREATE UNIQUE INDEX uq_file_watch_sightings
    ON file_watch_sightings(job_source, job_name, path, size_bytes, mtime);

CREATE UNIQUE INDEX idx_entity_codes_live
    ON entity_codes(kind, source, name) WHERE deleted_at IS NULL;

-- Satellite PKs restored to their pre-1050 name-embedding shapes. Same refusal
-- property: duplicate-named siblings' satellite rows collide on the restored
-- PKs and fail the rollback loudly.

CREATE TABLE definition_schedules_old (
    owner_source TEXT NOT NULL DEFAULT 'git' CHECK (owner_source IN ('git','cronomicon')),
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name   TEXT NOT NULL,
    name         TEXT NOT NULL,
    cron         TEXT NOT NULL,
    env          TEXT,
    position     INTEGER NOT NULL DEFAULT 0,
    source_ref   TEXT,
    start_at     TEXT,
    end_at       TEXT,
    interval     TEXT,
    skip_calendars TEXT,
    only_calendars TEXT,
    owner_uid    TEXT,
    schedule_uid TEXT,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name)
);
INSERT INTO definition_schedules_old SELECT
    owner_source, owner_kind, owner_name, name, cron, env, position, source_ref,
    start_at, end_at, interval, skip_calendars, only_calendars, owner_uid, schedule_uid
FROM definition_schedules;
DROP TABLE definition_schedules;
ALTER TABLE definition_schedules_old RENAME TO definition_schedules;
CREATE INDEX idx_def_schedules_owner ON definition_schedules(owner_source, owner_kind, owner_name);
CREATE INDEX idx_def_schedules_owner_uid ON definition_schedules(owner_uid);

CREATE TABLE paused_jobs_old (
    source     TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    owner_kind TEXT NOT NULL DEFAULT 'job' CHECK (owner_kind IN ('job','workflow')),
    name       TEXT NOT NULL,
    paused_by  TEXT NOT NULL,
    paused_at  TEXT NOT NULL,
    owner_uid  TEXT,
    PRIMARY KEY (source, owner_kind, name)
);
INSERT INTO paused_jobs_old SELECT source, owner_kind, name, paused_by, paused_at, owner_uid FROM paused_jobs;
DROP TABLE paused_jobs;
ALTER TABLE paused_jobs_old RENAME TO paused_jobs;
CREATE INDEX idx_paused_jobs_owner_uid ON paused_jobs(owner_uid);

CREATE TABLE reactions_old (
    owner_source  TEXT NOT NULL DEFAULT 'git' CHECK (owner_source IN ('git','cronomicon')),
    owner_kind    TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name    TEXT NOT NULL,
    name          TEXT NOT NULL,
    on_source     TEXT NOT NULL DEFAULT 'git' CHECK (on_source IN ('git','cronomicon')),
    on_kind       TEXT NOT NULL CHECK (on_kind IN ('job','workflow')),
    on_name       TEXT NOT NULL,
    on_outcome    TEXT NOT NULL CHECK (on_outcome IN ('success','failure','stopped','any')),
    delay_seconds INTEGER NOT NULL DEFAULT 0 CHECK (delay_seconds >= 0),
    min_interval_seconds INTEGER NOT NULL DEFAULT 0 CHECK (min_interval_seconds >= 0),
    include_workflow_children INTEGER NOT NULL DEFAULT 0 CHECK (include_workflow_children IN (0,1)),
    enabled       INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    position      INTEGER NOT NULL DEFAULT 0,
    owner_uid     TEXT,
    on_uid        TEXT,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name)
);
INSERT INTO reactions_old SELECT
    owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
    delay_seconds, min_interval_seconds, include_workflow_children, enabled, position,
    owner_uid, on_uid
FROM reactions;
DROP TABLE reactions;
ALTER TABLE reactions_old RENAME TO reactions;
CREATE INDEX idx_reactions_on ON reactions(on_kind, on_source, on_name, enabled);
CREATE INDEX idx_reactions_owner ON reactions(owner_kind, owner_source, owner_name);
CREATE INDEX idx_reactions_owner_uid ON reactions(owner_uid);
CREATE INDEX idx_reactions_on_uid ON reactions(on_uid);

CREATE TABLE reaction_deliveries_old (
    owner_source TEXT NOT NULL,
    owner_kind   TEXT NOT NULL,
    owner_name   TEXT NOT NULL,
    name         TEXT NOT NULL,
    src_kind     TEXT NOT NULL CHECK (src_kind IN ('job','workflow')),
    src_run_id   TEXT NOT NULL,
    outcome      TEXT NOT NULL,
    result       TEXT NOT NULL CHECK (result IN (
                     'pending','fired','error','expired',
                     'suppressed_disabled','suppressed_paused','suppressed_calendar',
                     'suppressed_depth','suppressed_rate')),
    detail       TEXT,
    pending_run_id TEXT,
    delivered_at TEXT NOT NULL,
    owner_uid    TEXT,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name, src_kind, src_run_id)
);
INSERT INTO reaction_deliveries_old SELECT
    owner_source, owner_kind, owner_name, name, src_kind, src_run_id, outcome, result,
    detail, pending_run_id, delivered_at, owner_uid
FROM reaction_deliveries;
DROP TABLE reaction_deliveries;
ALTER TABLE reaction_deliveries_old RENAME TO reaction_deliveries;
CREATE INDEX idx_reaction_deliveries_src ON reaction_deliveries(src_run_id);
CREATE INDEX idx_reaction_deliveries_fired
    ON reaction_deliveries(owner_source, owner_kind, owner_name, name, delivered_at)
    WHERE result = 'fired';
CREATE INDEX idx_reaction_deliveries_owner_uid ON reaction_deliveries(owner_uid);

CREATE TABLE reference_bindings_old (
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','script')),
    owner_source TEXT NOT NULL DEFAULT '',
    owner_name   TEXT NOT NULL,
    ref_kind     TEXT NOT NULL CHECK (ref_kind IN ('secret','var','key')),
    ref_name     TEXT NOT NULL,
    alias        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    owner_uid    TEXT,
    PRIMARY KEY (owner_kind, owner_source, owner_name, ref_kind, ref_name, alias)
);
INSERT INTO reference_bindings_old SELECT
    owner_kind, owner_source, owner_name, ref_kind, ref_name, alias, created_by, created_at, owner_uid
FROM reference_bindings;
DROP TABLE reference_bindings;
ALTER TABLE reference_bindings_old RENAME TO reference_bindings;
CREATE INDEX idx_reference_bindings_owner ON reference_bindings(owner_kind, owner_source, owner_name);
CREATE INDEX idx_reference_bindings_owner_uid ON reference_bindings(owner_uid);

-- ── triggers (created LAST: an ALTER TABLE RENAME re-parses every trigger,
-- so none may exist while a table it references is mid-rebuild) ────────────

CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;
CREATE TRIGGER trg_paused_jobs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;
CREATE TRIGGER trg_pending_runs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM pending_runs
    WHERE kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;
CREATE TRIGGER reference_bindings_job_cleanup AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;
CREATE TRIGGER trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'job'
       AND (owner_uid = OLD.uid
         OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'workflow'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;
CREATE TRIGGER trg_paused_jobs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'workflow'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;
CREATE TRIGGER trg_pending_runs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM pending_runs
    WHERE kind = 'workflow'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;
CREATE TRIGGER trg_reactions_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'workflow'
       AND (owner_uid = OLD.uid
         OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;
CREATE TRIGGER reference_bindings_script_cleanup
AFTER DELETE ON scripts
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'script' AND owner_name = OLD.name;
END;

DROP INDEX IF EXISTS uq_definition_revisions;
CREATE UNIQUE INDEX uq_definition_revisions
    ON definition_revisions(kind, source, name, revision_no);
