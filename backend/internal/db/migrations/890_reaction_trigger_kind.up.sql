-- 890 trigger_kind = 'reaction' on runs and workflow_runs
-- (the reactions-update plan, RX-3 — Phase B).
--
-- A reaction is a first-class trigger kind, alongside manual/scheduled/webhook,
-- so a reaction-fired run must be able to SAY it was one. Without this, History
-- cannot answer "why did this run?" for the entire feature, and the reactor
-- cannot enqueue at all — the CHECK would reject the insert.
--
-- SQLite cannot alter a CHECK constraint, so both tables are rebuilt.
--
-- ⚠️ The two vocabularies differ TODAY and are deliberately left differing:
-- runs.trigger_kind has 'workflow' and workflow_runs.trigger_kind does not
-- (a workflow is never triggered by a workflow — there is no nesting). This
-- migration adds ONE value to each and opportunistically unifies nothing.
-- Normalising them here would be an unreviewed semantic change riding in on a
-- mechanical one.
--
-- ══ THE TWO WAYS THIS MIGRATION CAN SILENTLY DESTROY DATA ══════════════════
--
-- This pool opens with _foreign_keys=on (db.go), so FK enforcement IS LIVE
-- during migrations. Migration 490's header claims otherwise ("golang-migrate
-- wraps each migration in a transaction where PRAGMA foreign_keys is a no-op");
-- that claim was written when jobs/runs/scripts had no inbound foreign keys and
-- migration 830 already had to retract it. It is wrong for `runs` today.
--
-- SQLite's DROP TABLE performs an implicit DELETE FROM first, which fires
-- cascade actions. So:
--
--   1. DROP TABLE runs  →  ON DELETE CASCADE wipes EVERY ROW of run_agencies
--      (690). That table is the runner claim query's isolation index; losing it
--      makes every run unclaimable by every departmental runner.
--
--   2. DROP TABLE workflow_runs  →  ON DELETE SET NULL blanks
--      runs.workflow_run_id for EVERY RUN, severing workflow→run linkage across
--      all of History. The rename puts the table back; the linkage does not
--      come back with it.
--
-- Both are stashed below and restored after. This is the 830 recipe, and 830's
-- own comment is the warning worth repeating: the stash "is the only reason
-- this is not a silent loss" — the failure is invisible without a data test,
-- because an empty child table and a NULL column are both perfectly valid.

-- ── stash the two things the rebuilds would destroy ────────────────────────
CREATE TABLE _890_run_agencies AS SELECT * FROM run_agencies;
-- Only the pairing is needed; runs.id survives the rebuild unchanged, so the
-- stash re-resolves.
CREATE TABLE _890_run_wf AS
    SELECT id, workflow_run_id FROM runs WHERE workflow_run_id IS NOT NULL;

-- ── runs → widen trigger_kind ──────────────────────────────────────────────
-- The column list is taken from the LIVE schema, not from 490's template: that
-- template is nine columns stale (it predates 511/512/610/620/680/710/750/870
-- and still carries `agency`, which 700 dropped). A rebuild that drops a column
-- is not recoverable from the down-migration.
CREATE TABLE runs_new (
    id              TEXT PRIMARY KEY,
    job_name        TEXT NOT NULL,
    run_type        TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl','python')),
    scope           TEXT,
    target_host     TEXT,
    status          TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    queued_reason   TEXT,
    triggered_by    TEXT NOT NULL,
    trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','workflow','webhook','reaction')),
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
INSERT INTO runs_new (id, job_name, run_type, scope, target_host, status, queued_reason, triggered_by,
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
ALTER TABLE runs_new RENAME TO runs;

-- All nine, from the live schema. Copying 490's list would silently lose
-- idx_runs_claimable (690) — the difference between an index seek and a full
-- SCAN runs on EVERY runner poll — and the two calendar/reaction partials.
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

-- ── workflow_runs → widen trigger_kind ─────────────────────────────────────
-- Never rebuilt before this migration; the column list below is the live one
-- (001 base + 030/090/170/240/260/870/880 additions).
CREATE TABLE workflow_runs_new (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('queued','running','success','failure','warning','killed','skipped')),
    triggered_by  TEXT NOT NULL,
    -- 'workflow' is deliberately absent: see the header.
    trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','webhook','reaction')),
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
INSERT INTO workflow_runs_new (id, workflow_name, status, triggered_by, trigger_kind, started_at,
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
ALTER TABLE workflow_runs_new RENAME TO workflow_runs;

CREATE INDEX idx_workflow_runs_source_name
    ON workflow_runs (workflow_source, workflow_name);
CREATE INDEX idx_workflow_runs_suppressed_by_calendar
    ON workflow_runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
CREATE INDEX idx_workflow_runs_reacted_to
    ON workflow_runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;

-- ── restore what the drops destroyed ───────────────────────────────────────
-- Order matters: workflow_run_id is restored AFTER the workflow_runs rebuild,
-- or the FK would reject rows pointing at a table mid-replacement.
UPDATE runs
   SET workflow_run_id = (SELECT s.workflow_run_id FROM _890_run_wf s WHERE s.id = runs.id)
 WHERE id IN (SELECT id FROM _890_run_wf);

INSERT OR IGNORE INTO run_agencies SELECT * FROM _890_run_agencies;

DROP TABLE _890_run_wf;
DROP TABLE _890_run_agencies;
