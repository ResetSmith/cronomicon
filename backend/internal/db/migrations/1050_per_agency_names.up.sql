-- 1050_per_agency_names — AF-4b stage R2-5: the uid becomes the PRIMARY KEY of
-- jobs, workflows and schedules, and name uniqueness relaxes to per-agency
-- (the rbac2 plan). The rebuild stages R2-1..R2-4 existed to make
-- safe: every referencing edge already carries the uid, so nothing here moves
-- an edge — only the constraint changes.
--
-- THE CONSTRAINT SHAPE is deliberately three different answers, not one:
--
--   * schedules KEEP full UNIQUE(source, name) — R2-Q1: a schedule has no
--     scope, so "the agencies it belongs to" is not even definable, and the
--     name-addressed /schedules/{name}?source= route stays valid.
--   * the git pool keeps a PARTIAL UNIQUE(source, name) WHERE source='git' —
--     YAML names live in one repository and cannot duplicate anyway; keeping
--     the constraint lets sync keep its atomic upsert (ON CONFLICT against the
--     partial index), and the sync validator refuses a duplicate pair in the
--     repo BEFORE it gets here.
--   * the cronomicon pool has NO unique (source, name): per-agency uniqueness is
--     a SET-OVERLAP rule ("unique within EACH agency the scope maps to; the
--     All pool overlaps everything"), which no SQLite UNIQUE can express. It
--     is enforced as a checked invariant in the compose/sync/restore write
--     paths, and the schema's job is only to not contradict it.
--
-- THE TRIGGERS DROP THEIR NAME ARM here, as 1020's header promised. The arm
-- was "precisely correct while names are unique" — that clause is void as of
-- this migration: a NULL-uid satellite row matching OLD.name could now belong
-- to a same-named SIBLING, so the arm that used to prevent orphans would start
-- deleting another definition's rows. uid is NOT NULL for every definition
-- (backfilled below, PK from here on) and every satellite writer stamps it
-- (R2-2's tests), so the arm has nothing left to catch.
--
-- THE TWO DEFERRED INDEX SWAPS from R2-2 land here, where they are REQUIRED
-- rather than merely tidy:
--   * uq_file_watch_sightings re-keys on job_uid — under duplicate names the
--     name-keyed de-dupe would treat two different jobs' watches on one path
--     as the same arrival, silently swallowing one of them.
--   * entity_codes' live-unique moves to (kind, uid) — the name-keyed one
--     would refuse a second same-named job its own log-folder code (or worse,
--     hand it the sibling's). The name-keyed index is DROPPED; the allocator
--     is uid-keyed as of this release.
--
-- Rebuild pattern per 170/940: create _new, copy, drop, rename, recreate every
-- index and trigger (DROP TABLE discards them — the 940 lesson). No inbound
-- FKs reference these tables (the AF-4a inventory: zero real FKs), so there is
-- no 950-style stash/restore to do.

-- The scripts-table trigger (820) references reference_bindings; it must not
-- exist while that table is mid-rebuild (ALTER RENAME re-parses every trigger).
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;

-- Safety: no row may enter the rebuild uid-less.
UPDATE jobs      SET uid = lower(hex(randomblob(16))) WHERE uid IS NULL OR uid = '';
UPDATE workflows SET uid = lower(hex(randomblob(16))) WHERE uid IS NULL OR uid = '';
UPDATE schedules SET uid = lower(hex(randomblob(16))) WHERE uid IS NULL OR uid = '';

-- ── jobs ────────────────────────────────────────────────────────────────────

CREATE TABLE jobs_new (
    uid                TEXT PRIMARY KEY,
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
    watch_json         TEXT
);

INSERT INTO jobs_new SELECT
    uid, name, source, run_type, description, scope, target_host, schedule, tags,
    enabled, timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
    source_path, synced_at, command, script, script_path, executor, script_ref,
    content_hash, created_by, created_at, last_modified_by, last_modified_at,
    env_json, backoff_seconds, continue_on_error, prompts_json, env_passthrough,
    project_root, requires_json, prompt_enforcement, ssh_user, ssh_credential,
    become_password_secret, deleted_at, deleted_by, warn_after_seconds,
    must_finish_by, watch_json
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX idx_jobs_schedule   ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_jobs_deleted    ON jobs(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX idx_jobs_source_name ON jobs(source, name);
CREATE UNIQUE INDEX uq_jobs_git_name ON jobs(source, name) WHERE source = 'git';


-- ── workflows ───────────────────────────────────────────────────────────────

CREATE TABLE workflows_new (
    uid              TEXT PRIMARY KEY,
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
    deleted_by       TEXT
);

INSERT INTO workflows_new SELECT
    uid, name, source, description, steps, schedule, enabled, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, layout_json, deleted_at, deleted_by
FROM workflows;

DROP TABLE workflows;
ALTER TABLE workflows_new RENAME TO workflows;

CREATE INDEX idx_workflows_deleted ON workflows(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX idx_workflows_source_name ON workflows(source, name);
CREATE UNIQUE INDEX uq_workflows_git_name ON workflows(source, name) WHERE source = 'git';


-- ── schedules ───────────────────────────────────────────────────────────────

CREATE TABLE schedules_new (
    uid              TEXT PRIMARY KEY,
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
    UNIQUE (source, name)
);

INSERT INTO schedules_new SELECT
    uid, name, source, description, cron, env, content_hash, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, start_at, end_at, interval, skip_calendars, only_calendars,
    deleted_at, deleted_by
FROM schedules;

DROP TABLE schedules;
ALTER TABLE schedules_new RENAME TO schedules;

CREATE INDEX idx_schedules_deleted ON schedules(deleted_at) WHERE deleted_at IS NOT NULL;

-- ── the two deferred de-dupe swaps ──────────────────────────────────────────

DROP INDEX IF EXISTS uq_file_watch_sightings;
CREATE UNIQUE INDEX uq_file_watch_sightings
    ON file_watch_sightings(job_uid, path, size_bytes, mtime)
    WHERE job_uid IS NOT NULL;
-- Legacy NULL-uid rows (their job vanished before 1020) fall outside the
-- constraint; every post-1020 writer stamps the uid, so new arrivals always
-- carry it. The name-keyed READ index stays for the history endpoint.

DROP INDEX IF EXISTS idx_entity_codes_live;
-- idx_entity_codes_live_uid (from 1020) is now THE live-unique constraint.

-- The revision chain is per-identity now: a purge-then-recreate is a NEW
-- identity whose chain restarts at #1, and two siblings' chains interleave by
-- name. The name-keyed unique (920) would refuse both.
DROP INDEX IF EXISTS uq_definition_revisions;
CREATE UNIQUE INDEX uq_definition_revisions
    ON definition_revisions(kind, uid, revision_no)
    WHERE uid IS NOT NULL;

-- ── satellite PKs re-key (the R2-5 completion the inventory missed) ─────────
--
-- Five satellite tables embed the OWNER NAME in their PRIMARY KEY, which would
-- physically forbid two same-named siblings from having their own pause,
-- schedule entries, reactions, deliveries or bindings — an INSERT for the
-- second sibling collides with the first. Each becomes a rowid table whose
-- uniqueness keys on the owner's uid. NULL-uid rows (owners that vanished
-- pre-1020) fall outside the constraints via NULL-distinctness; every current
-- writer stamps the uid. reference_bindings keeps a name-keyed partial unique
-- for its SCRIPT owners, whose identity is deliberately still the name.

CREATE TABLE definition_schedules_new (
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
    schedule_uid TEXT
);
INSERT INTO definition_schedules_new SELECT
    owner_source, owner_kind, owner_name, name, cron, env, position, source_ref,
    start_at, end_at, interval, skip_calendars, only_calendars, owner_uid, schedule_uid
FROM definition_schedules;
DROP TABLE definition_schedules;
ALTER TABLE definition_schedules_new RENAME TO definition_schedules;
CREATE INDEX idx_def_schedules_owner ON definition_schedules(owner_source, owner_kind, owner_name);
CREATE INDEX idx_def_schedules_owner_uid ON definition_schedules(owner_uid);
CREATE UNIQUE INDEX uq_def_schedules_entry ON definition_schedules(owner_kind, owner_uid, name);

CREATE TABLE paused_jobs_new (
    source     TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    owner_kind TEXT NOT NULL DEFAULT 'job' CHECK (owner_kind IN ('job','workflow')),
    name       TEXT NOT NULL,
    paused_by  TEXT NOT NULL,
    paused_at  TEXT NOT NULL,
    owner_uid  TEXT
);
INSERT INTO paused_jobs_new SELECT source, owner_kind, name, paused_by, paused_at, owner_uid FROM paused_jobs;
DROP TABLE paused_jobs;
ALTER TABLE paused_jobs_new RENAME TO paused_jobs;
CREATE INDEX idx_paused_jobs_owner_uid ON paused_jobs(owner_uid);
CREATE UNIQUE INDEX uq_paused_owner ON paused_jobs(owner_kind, owner_uid);

CREATE TABLE reactions_new (
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
    on_uid        TEXT
);
INSERT INTO reactions_new SELECT
    owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
    delay_seconds, min_interval_seconds, include_workflow_children, enabled, position,
    owner_uid, on_uid
FROM reactions;
DROP TABLE reactions;
ALTER TABLE reactions_new RENAME TO reactions;
CREATE INDEX idx_reactions_on ON reactions(on_kind, on_source, on_name, enabled);
CREATE INDEX idx_reactions_owner ON reactions(owner_kind, owner_source, owner_name);
CREATE INDEX idx_reactions_owner_uid ON reactions(owner_uid);
CREATE INDEX idx_reactions_on_uid ON reactions(on_uid);
CREATE UNIQUE INDEX uq_reactions_entry ON reactions(owner_kind, owner_uid, name);

CREATE TABLE reaction_deliveries_new (
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
    owner_uid    TEXT
);
INSERT INTO reaction_deliveries_new SELECT
    owner_source, owner_kind, owner_name, name, src_kind, src_run_id, outcome, result,
    detail, pending_run_id, delivered_at, owner_uid
FROM reaction_deliveries;
DROP TABLE reaction_deliveries;
ALTER TABLE reaction_deliveries_new RENAME TO reaction_deliveries;
CREATE INDEX idx_reaction_deliveries_src ON reaction_deliveries(src_run_id);
CREATE INDEX idx_reaction_deliveries_fired
    ON reaction_deliveries(owner_kind, owner_uid, name, delivered_at)
    WHERE result = 'fired';
CREATE INDEX idx_reaction_deliveries_owner_uid ON reaction_deliveries(owner_uid);
-- The idempotency claim (INSERT OR IGNORE lands here).
CREATE UNIQUE INDEX uq_reaction_delivery ON reaction_deliveries(owner_kind, owner_uid, name, src_kind, src_run_id);

CREATE TABLE reference_bindings_new (
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','script')),
    owner_source TEXT NOT NULL DEFAULT '',
    owner_name   TEXT NOT NULL,
    ref_kind     TEXT NOT NULL CHECK (ref_kind IN ('secret','var','key')),
    ref_name     TEXT NOT NULL,
    alias        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    owner_uid    TEXT
);
INSERT INTO reference_bindings_new SELECT
    owner_kind, owner_source, owner_name, ref_kind, ref_name, alias, created_by, created_at, owner_uid
FROM reference_bindings;
DROP TABLE reference_bindings;
ALTER TABLE reference_bindings_new RENAME TO reference_bindings;
CREATE INDEX idx_reference_bindings_owner ON reference_bindings(owner_kind, owner_source, owner_name);
CREATE INDEX idx_reference_bindings_owner_uid ON reference_bindings(owner_uid);
CREATE UNIQUE INDEX uq_reference_bindings_job
    ON reference_bindings(owner_kind, owner_uid, ref_kind, ref_name, alias)
    WHERE owner_uid IS NOT NULL;
CREATE UNIQUE INDEX uq_reference_bindings_script
    ON reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, alias)
    WHERE owner_uid IS NULL;

-- ── triggers (created LAST: an ALTER TABLE RENAME re-parses every trigger,
-- so none may exist while a table it references is mid-rebuild) ────────────

CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules WHERE owner_kind = 'job' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_paused_jobs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM paused_jobs WHERE owner_kind = 'job' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_pending_runs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM pending_runs WHERE kind = 'job' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER reference_bindings_job_cleanup AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings WHERE owner_kind = 'job' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions WHERE owner_kind = 'job' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules WHERE owner_kind = 'workflow' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_paused_jobs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM paused_jobs WHERE owner_kind = 'workflow' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_pending_runs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM pending_runs WHERE kind = 'workflow' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER trg_reactions_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM reactions WHERE owner_kind = 'workflow' AND owner_uid = OLD.uid;
END;
CREATE TRIGGER reference_bindings_script_cleanup
AFTER DELETE ON scripts
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'script' AND owner_name = OLD.name;
END;
