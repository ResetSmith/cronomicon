-- 170 Dual-source foundation (amadeus-v20.md — A9, Phase 2).
--
-- Jobs, Workflows, and Schedules may originate in Git (canonical, MR-reviewed) OR
-- in Cronomicon's DB (operator-authored in-app). Each carries `source ∈ {git,amadeus}`
-- and the runtime key becomes `(source, name)` (A9 — relaxes A2, scoping it to
-- git-source rows). This is the **repo's first SQLite table-rebuild migration** —
-- ALTER TABLE cannot change a PRIMARY KEY, so jobs/workflows/paused_jobs/
-- definition_schedules are rebuilt (create_new → copy → drop → rename) per the
-- SQLite 12-step procedure. Existing rows are all Git-origin → source='git'.
--
-- FK note: golang-migrate wraps each migration in a transaction, where
-- `PRAGMA foreign_keys` is a no-op. The only real FK in the schema is
-- runs.workflow_run_id → workflow_runs(id); workflow_runs is NOT rebuilt here
-- (column-add only), so the rebuilt tables have no inbound FK to violate.
--
-- The rebuild silently drops dependent triggers/indexes (the 090 cascade triggers
-- live ON jobs/workflows; idx_jobs_schedule/idx_jobs_script_ref are ON jobs) — all
-- are recreated below, the triggers now source-aware (Q-G / §13.1).

-- ── jobs → PK (source, name) ────────────────────────────────────────────────
CREATE TABLE jobs_new (
    name               TEXT NOT NULL,
    source             TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','amadeus')),
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
    synced_at          TEXT,                      -- was NOT NULL; relaxed so amadeus rows (no git sync) can omit it
    command            TEXT,
    script             TEXT,
    script_path        TEXT,
    executor           TEXT CHECK (executor IN ('runner','ssh')),
    script_ref         TEXT,
    content_hash       TEXT,
    -- S4 provenance for operator-authored (source='amadeus') rows.
    created_by         TEXT,
    created_at         TEXT,
    last_modified_by   TEXT,
    last_modified_at   TEXT,
    PRIMARY KEY (source, name)
);
INSERT INTO jobs_new (name, source, run_type, description, scope, target_host, schedule, tags,
                      enabled, timeout_seconds, retries, requestable, concurrency_policy,
                      concurrency_key, sensitive_logging, source_path, synced_at,
                      command, script, script_path, executor, script_ref, content_hash)
    SELECT name, 'git', run_type, description, scope, target_host, schedule, tags,
           enabled, timeout_seconds, retries, requestable, concurrency_policy,
           concurrency_key, sensitive_logging, source_path, synced_at,
           command, script, script_path, executor, script_ref, content_hash
    FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

-- ── workflows → PK (source, name) ───────────────────────────────────────────
CREATE TABLE workflows_new (
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','amadeus')),
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
    PRIMARY KEY (source, name)
);
INSERT INTO workflows_new (name, source, description, steps, schedule, enabled, source_path, synced_at)
    SELECT name, 'git', description, steps, schedule, enabled, source_path, synced_at FROM workflows;
DROP TABLE workflows;
ALTER TABLE workflows_new RENAME TO workflows;

-- ── paused_jobs → PK (source, owner_kind, name); retire the __wf__ sentinel ──
-- (Q-G) Workflows were paused as '__wf__'+name rows; an explicit owner_kind column
-- replaces the string-prefix hack. substr (not LIKE — '_' is a LIKE wildcard)
-- splits the legacy key.
CREATE TABLE paused_jobs_new (
    source     TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','amadeus')),
    owner_kind TEXT NOT NULL DEFAULT 'job' CHECK (owner_kind IN ('job','workflow')),
    name       TEXT NOT NULL,
    paused_by  TEXT NOT NULL,
    paused_at  TEXT NOT NULL,
    PRIMARY KEY (source, owner_kind, name)
);
INSERT INTO paused_jobs_new (source, owner_kind, name, paused_by, paused_at)
    SELECT 'git',
           CASE WHEN substr(job_name,1,6)='__wf__' THEN 'workflow' ELSE 'job' END,
           CASE WHEN substr(job_name,1,6)='__wf__' THEN substr(job_name,7) ELSE job_name END,
           paused_by, paused_at
    FROM paused_jobs;
DROP TABLE paused_jobs;
ALTER TABLE paused_jobs_new RENAME TO paused_jobs;

-- ── definition_schedules → PK (owner_source, owner_kind, owner_name, name) ───
CREATE TABLE definition_schedules_new (
    owner_source TEXT NOT NULL DEFAULT 'git' CHECK (owner_source IN ('git','amadeus')),
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name   TEXT NOT NULL,
    name         TEXT NOT NULL,
    cron         TEXT NOT NULL,
    env          TEXT,
    position     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name)
);
INSERT INTO definition_schedules_new (owner_source, owner_kind, owner_name, name, cron, env, position)
    SELECT 'git', owner_kind, owner_name, name, cron, env, position FROM definition_schedules;
DROP TABLE definition_schedules;
ALTER TABLE definition_schedules_new RENAME TO definition_schedules;

-- ── runs / workflow_runs — additive run-origin snapshot (no rebuild) ────────
ALTER TABLE runs ADD COLUMN job_source TEXT;            -- NULL ⇒ legacy/git run
ALTER TABLE workflow_runs ADD COLUMN workflow_source TEXT;

-- ── Recreate dropped indexes + source-aware cascade triggers ────────────────
CREATE INDEX idx_jobs_schedule ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_def_schedules_owner ON definition_schedules(owner_source, owner_kind, owner_name);

CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'job' AND owner_name = OLD.name AND owner_source = OLD.source;
END;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'workflow' AND owner_name = OLD.name AND owner_source = OLD.source;
END;
