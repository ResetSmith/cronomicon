-- 940_queue_policy — a Queue concurrency policy, and the removal of Replace (QP,
-- the prod-features plan §5).
--
-- WHAT CHANGES. concurrency_policy goes from ('Allow','Forbid','Replace') to
-- ('Allow','Forbid','Queue'). Both halves of that are deliberate.
--
--   * QUEUE is the missing option. Forbid loses the fire (recorded as a skip)
--     and Allow lets it overlap; neither is "wait your turn", which is what an
--     operator usually means. A Queue-policy fire that meets a held gate parks
--     as a pending_runs row and promotes when the gate clears.
--
--   * REPLACE is REMOVED because it never existed. It was parsed, stored,
--     validated, documented in the spec — and no code branch anywhere read it.
--     Every gate in the tree is `policy == "Forbid"`. So the live behaviour of a
--     Replace job was Allow, and shipping a fourth value beside a decorative
--     third was not an option worth taking (PF-Q14). Existing rows are coerced
--     to Allow BELOW, which changes nothing about how they actually ran.
--
-- ⚠️ THE REBUILD. SQLite cannot ALTER a CHECK, so `jobs` is rebuilt by the
-- 12-step procedure. Read 890's header before touching this. Two specifics:
--
--   * FK enforcement IS live during migrations (the pool opens with
--     _foreign_keys=on), but NO table holds an inbound foreign key to jobs —
--     runs references it by (job_name, job_source) TEXT with no constraint, on
--     purpose (710). So the DROP cascades nothing. Verified before writing.
--   * DROP TABLE does not fire DELETE triggers, but it DOES destroy them along
--     with the table's indexes. jobs carries FIVE triggers and THREE indexes as
--     of this migration — every one is recreated at the bottom, and a rebuild
--     that forgets one silently stops cascading a delete. 200's caveat named
--     two of them; there are five now.

-- Coerce the decorative value before the new CHECK would reject it. This is a
-- no-op behaviourally: nothing ever branched on Replace.
UPDATE jobs SET concurrency_policy = 'Allow' WHERE concurrency_policy = 'Replace';

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
    PRIMARY KEY (source, name)
);

INSERT INTO jobs_new (
    name, source, run_type, description, scope, target_host, schedule, tags, enabled,
    timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
    source_path, synced_at, command, script, script_path, executor, script_ref, content_hash,
    created_by, created_at, last_modified_by, last_modified_at, env_json,
    backoff_seconds, continue_on_error, prompts_json, env_passthrough, project_root,
    requires_json, prompt_enforcement, ssh_user, ssh_credential, become_password_secret,
    deleted_at, deleted_by, warn_after_seconds, must_finish_by)
SELECT
    name, source, run_type, description, scope, target_host, schedule, tags, enabled,
    timeout_seconds, retries, requestable, concurrency_policy, concurrency_key,
    source_path, synced_at, command, script, script_path, executor, script_ref, content_hash,
    created_by, created_at, last_modified_by, last_modified_at, env_json,
    backoff_seconds, continue_on_error, prompts_json, env_passthrough, project_root,
    requires_json, prompt_enforcement, ssh_user, ssh_credential, become_password_secret,
    deleted_at, deleted_by, warn_after_seconds, must_finish_by
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

-- Indexes (3).
CREATE INDEX idx_jobs_schedule ON jobs(schedule);
CREATE INDEX idx_jobs_script_ref ON jobs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_jobs_deleted ON jobs(deleted_at) WHERE deleted_at IS NOT NULL;

-- Delete-cascade triggers (5). Losing any one of these means a deleted job
-- leaves orphans that the next job of the same name inherits.
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

-- ── The queue itself ────────────────────────────────────────────────────────
--
-- A queued run is a pending_run whose fire condition is "the gate cleared"
-- rather than "the clock reached T". Both columns are additive (the 880
-- precedent) rather than widening pending_runs.status's two-value CHECK, which
-- would mean a second rebuild for no gain.
--
-- concurrency_key is a REAL column rather than a params_json read because the
-- cap count and the promotion probe both key on it, and JSON-parsing every
-- pending row on every 15-second pass to find it is not a query.
ALTER TABLE pending_runs ADD COLUMN concurrency_key TEXT;

-- gate_kind distinguishes why a row is parked. NULL = the clock (ad-hoc runs
-- from AR, delayed reactions from RX — the existing meaning, so old rows read
-- correctly with no backfill). 'concurrency' = queued behind a held gate.
ALTER TABLE pending_runs ADD COLUMN gate_kind TEXT;

-- The queue-depth count and the per-key probe.
CREATE INDEX idx_pending_runs_queue ON pending_runs(concurrency_key, status)
    WHERE concurrency_key IS NOT NULL;

-- ── Priority ────────────────────────────────────────────────────────────────
--
-- Per-trigger only (PF-Q11): a spec-level default invites starvation, where one
-- job's standing priority permanently outranks another's. Additive, no rebuild.
ALTER TABLE runs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;

-- The claim query's covering index, re-cut to lead with priority.
--
-- ⚠️ The column ORDER MATTERS and so does the direction. Claims are
-- `ORDER BY priority DESC, created_at ASC` — highest first, then oldest — and a
-- single-direction index cannot serve a mixed-direction sort. SQLite has
-- supported per-column DESC in indexes since 3.3, so the index is cut to match
-- the sort exactly. There are TWO claim queries (runner/poll.go and
-- sshexec/sshexec.go) and both use this shape; poll_claim_bench_test.go exists
-- because this query's plan is load-bearing.
DROP INDEX IF EXISTS idx_runs_claimable;
CREATE INDEX idx_runs_claimable ON runs (status, executor, priority DESC, created_at ASC);
