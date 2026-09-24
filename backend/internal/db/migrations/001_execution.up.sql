-- 001 execution domain: runs, workflow runs, activity feed, change log, schedule pushes.
-- Timestamps are ISO-8601 TEXT on the wire and at rest (S12). IDs are backend-minted
-- UUIDv7 TEXT (S11). Booleans are INTEGER 0/1. Enums are TEXT + CHECK.

CREATE TABLE workflow_runs (
    id            TEXT PRIMARY KEY,                 -- UUIDv7 trace id (S11)
    workflow_name TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    triggered_by  TEXT NOT NULL,                    -- authenticated identity (S6)
    trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','webhook')),
    started_at    TEXT,
    completed_at  TEXT,
    created_at    TEXT NOT NULL
);

CREATE TABLE runs (
    id              TEXT PRIMARY KEY,               -- UUIDv7 trace id (S11)
    job_name        TEXT NOT NULL,
    run_type        TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl')),
    scope           TEXT,
    target_host     TEXT,
    status          TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    queued_reason   TEXT,                           -- e.g. 'waiting for terraform-capable runner' (A6.3)
    triggered_by    TEXT NOT NULL,                  -- authenticated identity (S6)
    trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','workflow','webhook')),
    killed_by       TEXT,
    -- S16: concurrency policy resolved at trigger time; key defaults to job_name.
    concurrency_key TEXT,
    -- S3: real FK to the parent workflow run (replaces the string-match convention).
    workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE SET NULL,
    started_at      TEXT,
    completed_at    TEXT,
    duration_ms     INTEGER,
    exit_code       INTEGER,
    created_at      TEXT NOT NULL
);
CREATE INDEX idx_runs_job_name   ON runs(job_name);
CREATE INDEX idx_runs_status     ON runs(status);
CREATE INDEX idx_runs_created_at ON runs(created_at);
CREATE INDEX idx_runs_workflow   ON runs(workflow_run_id);

-- Activity feed: 7-kind discriminated union (architecture §; openapi /activity).
CREATE TABLE activity (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL CHECK (kind IN ('run-start','run-end','workflow-start','workflow-end','config','gitsync','push')),
    outcome       TEXT CHECK (outcome IN ('success','failure','warning')),
    actor         TEXT,
    job_name      TEXT,
    workflow_name TEXT,
    target        TEXT,
    scope         TEXT,
    schedule_file TEXT,
    category      TEXT,
    summary       TEXT,
    details       TEXT,
    trace_id      TEXT,                              -- soft link to runs/workflow_runs
    at            TEXT NOT NULL,
    created_at    TEXT NOT NULL
);
CREATE INDEX idx_activity_at   ON activity(at);
CREATE INDEX idx_activity_kind ON activity(kind);

-- Change log / audit trail of in-app config mutations.
CREATE TABLE change_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,                        -- authenticated identity (S6)
    category   TEXT NOT NULL,
    action     TEXT NOT NULL,
    target     TEXT,
    details    TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_change_log_at       ON change_log(at);
CREATE INDEX idx_change_log_category ON change_log(category);

-- Audit of Cronomicon→GitLab schedule pushes. base_sha is the A2 If-Match precondition.
CREATE TABLE schedule_pushes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    at            TEXT NOT NULL,
    actor         TEXT NOT NULL,
    schedule_file TEXT NOT NULL,
    base_sha      TEXT,                              -- the commit the edit was based on (A2/D2/D13)
    new_sha       TEXT,                              -- resulting commit on success
    status        TEXT NOT NULL CHECK (status IN ('success','rejected','failed')),
    trace_id      TEXT,
    details       TEXT,
    created_at    TEXT NOT NULL
);
CREATE INDEX idx_schedule_pushes_at ON schedule_pushes(at);
