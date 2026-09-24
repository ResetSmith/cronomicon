-- 005 job + workflow definition CACHE (read model).
--
-- GitLab is the source of truth for definitions (architecture §2.1). B3 parses
-- the YAML from git and upserts these cache rows on sync; B5 (scheduler/engine)
-- and the read APIs query them. This table is the DB-level integration seam that
-- lets the GitLab, scheduler, and execution slices stay decoupled (no cross-pkg
-- imports) — everything meets on shared tables.

CREATE TABLE jobs (
    name               TEXT PRIMARY KEY,
    run_type           TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl')),
    description        TEXT,
    scope              TEXT,
    target_host        TEXT,
    schedule           TEXT,                          -- cron expression (nullable = manual only)
    tags               TEXT NOT NULL DEFAULT '[]',    -- JSON array
    enabled            INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    timeout_seconds    INTEGER,
    retries            INTEGER NOT NULL DEFAULT 0,
    -- A7: jobs are not self-service-requestable in v1 (run_requests deferred to V2-2).
    requestable        INTEGER NOT NULL DEFAULT 0 CHECK (requestable IN (0,1)),
    -- S16: per-job concurrency. policy default Allow; key defaults to the job name.
    concurrency_policy TEXT NOT NULL DEFAULT 'Allow' CHECK (concurrency_policy IN ('Allow','Forbid','Replace')),
    concurrency_key    TEXT,
    sensitive_logging  INTEGER NOT NULL DEFAULT 0 CHECK (sensitive_logging IN (0,1)), -- S7
    source_path        TEXT,                          -- jobs/<name>.yaml in git
    synced_at          TEXT NOT NULL
);
CREATE INDEX idx_jobs_schedule ON jobs(schedule);

CREATE TABLE workflows (
    name        TEXT PRIMARY KEY,
    description TEXT,
    steps       TEXT NOT NULL DEFAULT '[]',           -- JSON: ordered steps + branch conditions
    schedule    TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    source_path TEXT,                                 -- workflows/<name>.yaml in git
    synced_at   TEXT NOT NULL
);

-- Git sync bookkeeping: last-seen commit per ref, used for webhook-driven sync
-- and the A2 base_sha precondition baseline.
CREATE TABLE git_sync_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    last_sha    TEXT,
    last_synced_at TEXT,
    last_status TEXT
);
