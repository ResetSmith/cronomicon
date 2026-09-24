-- Reverse 890 — narrow trigger_kind back, dropping 'reaction'.
--
-- ⚠️ A narrowing down-migration FAILS if any row already carries the value
-- being removed, which is the correct behaviour: silently rewriting a
-- reaction-fired run's provenance to 'manual' would put a lie in History. Delete
-- or re-label those rows first if you genuinely intend to roll back. This mirrors
-- 490's down-migration note for the python run type.
--
-- The same two FK hazards as the up-migration apply in full — run_agencies is
-- cascade-wiped by DROP TABLE runs, and runs.workflow_run_id is blanked by
-- DROP TABLE workflow_runs — so the same stash/restore is repeated here rather
-- than assumed to be an up-only concern.

CREATE TABLE _890_run_agencies AS SELECT * FROM run_agencies;
CREATE TABLE _890_run_wf AS
    SELECT id, workflow_run_id FROM runs WHERE workflow_run_id IS NOT NULL;

CREATE TABLE runs_old (
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
    checkout_sha           TEXT,
    checkout_entry         TEXT,
    requires_json          TEXT,
    injection_audited      BOOLEAN NOT NULL DEFAULT 0,
    injects_secret         BOOLEAN NOT NULL DEFAULT 0,
    agencies_json          TEXT NOT NULL DEFAULT '[]',
    entity_code            TEXT,
    ssh_user               TEXT,
    ssh_credential         TEXT,
    suppressed_by_calendar TEXT,
    reaction_depth         INTEGER NOT NULL DEFAULT 0,
    reacted_to_run_id      TEXT
);
INSERT INTO runs_old (id, job_name, run_type, scope, target_host, status, queued_reason, triggered_by,
                      trigger_kind, killed_by, concurrency_key, workflow_run_id, started_at, completed_at,
                      duration_ms, exit_code, created_at, runner_id, executor, schedule_name, env_json, kind,
                      script_ref, content_hash, job_source, outputs_json, override_json, log_raw_offset,
                      checkout_sha, checkout_entry, requires_json, injection_audited, injects_secret,
                      agencies_json, entity_code, ssh_user, ssh_credential, suppressed_by_calendar,
                      reaction_depth, reacted_to_run_id)
    SELECT id, job_name, run_type, scope, target_host, status, queued_reason, triggered_by,
           trigger_kind, killed_by, concurrency_key, workflow_run_id, started_at, completed_at,
           duration_ms, exit_code, created_at, runner_id, executor, schedule_name, env_json, kind,
           script_ref, content_hash, job_source, outputs_json, override_json, log_raw_offset,
           checkout_sha, checkout_entry, requires_json, injection_audited, injects_secret,
           agencies_json, entity_code, ssh_user, ssh_credential, suppressed_by_calendar,
           reaction_depth, reacted_to_run_id
    FROM runs;
DROP TABLE runs;
ALTER TABLE runs_old RENAME TO runs;

CREATE INDEX idx_runs_created_at ON runs(created_at);
CREATE INDEX idx_runs_job_name   ON runs(job_name);
CREATE INDEX idx_runs_script_ref ON runs(script_ref) WHERE script_ref IS NOT NULL;
CREATE INDEX idx_runs_status     ON runs(status);
CREATE INDEX idx_runs_workflow   ON runs(workflow_run_id);
CREATE UNIQUE INDEX uq_runs_active_concurrency
    ON runs (concurrency_key)
    WHERE concurrency_key IS NOT NULL AND (status = 'queued' OR status = 'running');
CREATE INDEX idx_runs_claimable ON runs (status, executor, created_at);
CREATE INDEX idx_runs_suppressed_by_calendar
    ON runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
CREATE INDEX idx_runs_reacted_to
    ON runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;

CREATE TABLE workflow_runs_old (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    triggered_by  TEXT NOT NULL,
    trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','webhook')),
    started_at    TEXT,
    completed_at  TEXT,
    created_at    TEXT NOT NULL,
    workflow_id            INTEGER,
    duration_ms            INTEGER,
    scope                  TEXT,
    schedule_name          TEXT,
    env_json               TEXT,
    workflow_source        TEXT,
    steps_snapshot         TEXT,
    steps_hash             TEXT,
    cancelled              INTEGER NOT NULL DEFAULT 0,
    cancelled_at           TEXT,
    suppressed_by_calendar TEXT,
    queued_reason          TEXT,
    reaction_depth         INTEGER NOT NULL DEFAULT 0,
    reacted_to_run_id      TEXT
);
INSERT INTO workflow_runs_old (id, workflow_name, status, triggered_by, trigger_kind, started_at,
                               completed_at, created_at, workflow_id, duration_ms, scope, schedule_name,
                               env_json, workflow_source, steps_snapshot, steps_hash, cancelled,
                               cancelled_at, suppressed_by_calendar, queued_reason, reaction_depth,
                               reacted_to_run_id)
    SELECT id, workflow_name, status, triggered_by, trigger_kind, started_at,
           completed_at, created_at, workflow_id, duration_ms, scope, schedule_name,
           env_json, workflow_source, steps_snapshot, steps_hash, cancelled,
           cancelled_at, suppressed_by_calendar, queued_reason, reaction_depth,
           reacted_to_run_id
    FROM workflow_runs;
DROP TABLE workflow_runs;
ALTER TABLE workflow_runs_old RENAME TO workflow_runs;

CREATE INDEX idx_workflow_runs_source_name
    ON workflow_runs (workflow_source, workflow_name);
CREATE INDEX idx_workflow_runs_suppressed_by_calendar
    ON workflow_runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
CREATE INDEX idx_workflow_runs_reacted_to
    ON workflow_runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;

UPDATE runs
   SET workflow_run_id = (SELECT s.workflow_run_id FROM _890_run_wf s WHERE s.id = runs.id)
 WHERE id IN (SELECT id FROM _890_run_wf);

INSERT OR IGNORE INTO run_agencies SELECT * FROM _890_run_agencies;

DROP TABLE _890_run_wf;
DROP TABLE _890_run_agencies;
