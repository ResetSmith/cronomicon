-- 490 Python run-type — widen the run_type CHECK on jobs/runs/scripts to include 'python'.
--
-- Python becomes a first-class SHELL-FAMILY / SSH-capable run-type (like
-- bash/perl/powershell; NOT runner-only like ansible/terraform). SQLite cannot
-- ALTER a CHECK constraint in place, so the three tables that constrain run_type
-- are rebuilt via the 12-step procedure (create _new → copy → drop → rename),
-- then their indexes and triggers are recreated. This mirrors migration 170 (the
-- repo's table-rebuild precedent) and heeds the 200 caveat: a jobs rebuild MUST
-- recreate BOTH trg_def_schedules_job_delete AND trg_paused_jobs_job_delete.
--
-- FK note (as in 170): golang-migrate wraps each migration in a transaction where
-- PRAGMA foreign_keys is a no-op; no table has an inbound FK to jobs/runs/scripts,
-- so the drop/rename is safe. runs' OUTBOUND FKs (workflow_runs, runners) are
-- reproduced verbatim in runs_new. Column lists match the live schema exactly
-- (post-320 jobs has no sensitive_logging; the ALTER-added tail columns are kept).
--
-- ⚠️ RETRACTION (added 2026-08-11) — the "PRAGMA foreign_keys is a no-op" half of
-- that note is WRONG and must not be copied into a new migration. The pool opens
-- with _foreign_keys=on (db.go), so FK enforcement IS live during migrations and
-- DROP TABLE fires cascades. The claim was never exercised HERE only because the
-- second half is true: jobs/runs/scripts had no inbound FKs at the time. Migration
-- 830 discovered the difference the hard way and 890's header documents the
-- stash-and-restore recipe a rebuild under an inbound FK requires. Read 890 before
-- rebuilding any table; this note's reasoning is safe only for this migration.

-- ── jobs → widen run_type CHECK ─────────────────────────────────────────────
CREATE TABLE jobs_new (
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
    PRIMARY KEY (source, name)
);
INSERT INTO jobs_new (name, source, run_type, description, scope, target_host, schedule, tags,
                      enabled, timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
                      source_path, synced_at, command, script, script_path, executor, script_ref, content_hash,
                      created_by, created_at, last_modified_by, last_modified_at,
                      env_json, backoff_seconds, continue_on_error, prompts_json)
    SELECT name, source, run_type, description, scope, target_host, schedule, tags,
           enabled, timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
           source_path, synced_at, command, script, script_path, executor, script_ref, content_hash,
           created_by, created_at, last_modified_by, last_modified_at,
           env_json, backoff_seconds, continue_on_error, prompts_json
    FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
CREATE INDEX idx_jobs_schedule ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
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

-- ── runs → widen run_type CHECK (outbound FKs reproduced) ───────────────────
CREATE TABLE runs_new (
    id              TEXT PRIMARY KEY,
    job_name        TEXT NOT NULL,
    run_type        TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl','python')),
    scope           TEXT,
    target_host     TEXT,
    status          TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    queued_reason   TEXT,
    triggered_by    TEXT NOT NULL,
    trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','workflow','webhook')),
    killed_by       TEXT,
    concurrency_key TEXT,
    workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE SET NULL,
    started_at      TEXT,
    completed_at    TEXT,
    duration_ms     INTEGER,
    exit_code       INTEGER,
    created_at      TEXT NOT NULL,
    runner_id       TEXT REFERENCES runners(id) ON DELETE SET NULL,
    executor        TEXT NOT NULL DEFAULT 'ssh' CHECK (executor IN ('runner','ssh')),
    schedule_name   TEXT,
    env_json        TEXT,
    kind            TEXT NOT NULL DEFAULT 'job' CHECK (kind IN ('job','ssh-test')),
    script_ref      TEXT,
    content_hash    TEXT,
    job_source      TEXT,
    outputs_json    TEXT,
    override_json   TEXT,
    log_raw_offset  INTEGER NOT NULL DEFAULT 0,
    agency          TEXT
);
INSERT INTO runs_new (id, job_name, run_type, scope, target_host, status, queued_reason, triggered_by,
                      trigger_kind, killed_by, concurrency_key, workflow_run_id, started_at, completed_at,
                      duration_ms, exit_code, created_at, runner_id, executor, schedule_name, env_json, kind,
                      script_ref, content_hash, job_source, outputs_json, override_json, log_raw_offset, agency)
    SELECT id, job_name, run_type, scope, target_host, status, queued_reason, triggered_by,
           trigger_kind, killed_by, concurrency_key, workflow_run_id, started_at, completed_at,
           duration_ms, exit_code, created_at, runner_id, executor, schedule_name, env_json, kind,
           script_ref, content_hash, job_source, outputs_json, override_json, log_raw_offset, agency
    FROM runs;
DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;
CREATE INDEX idx_runs_created_at ON runs(created_at);
CREATE INDEX idx_runs_job_name   ON runs(job_name);
CREATE INDEX idx_runs_script_ref ON runs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_runs_status     ON runs(status);
CREATE INDEX idx_runs_workflow   ON runs(workflow_run_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_runs_active_concurrency
ON runs (concurrency_key)
WHERE concurrency_key IS NOT NULL AND (status = 'queued' OR status = 'running');

-- ── scripts → widen run_type CHECK ──────────────────────────────────────────
CREATE TABLE scripts_new (
    name         TEXT PRIMARY KEY,
    description  TEXT,
    run_type     TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl','python')),
    command      TEXT,
    script       TEXT,
    script_path  TEXT,
    executor     TEXT CHECK (executor IN ('runner','ssh')),
    content_hash TEXT NOT NULL,
    source_path  TEXT,
    synced_at    TEXT NOT NULL,
    warnings     TEXT NOT NULL DEFAULT '[]',
    variables    TEXT NOT NULL DEFAULT '[]',
    tags         TEXT NOT NULL DEFAULT '[]'
);
INSERT INTO scripts_new (name, description, run_type, command, script, script_path, executor,
                         content_hash, source_path, synced_at, warnings, variables, tags)
    SELECT name, description, run_type, command, script, script_path, executor,
           content_hash, source_path, synced_at, warnings, variables, tags
    FROM scripts;
DROP TABLE scripts;
ALTER TABLE scripts_new RENAME TO scripts;
