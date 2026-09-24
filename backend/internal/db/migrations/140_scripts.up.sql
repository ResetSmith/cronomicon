-- 140 Scripts as a first-class primitive (scripts-plan.md, B-Git, amendment A8).
--
-- A Script is the reusable executable unit; a Job references one via script_ref
-- instead of embedding its body inline. Like jobs/workflows, GitLab is the source
-- of truth (architecture §2.1) and this `scripts` table is a Git-derived read-model
-- cache (same pattern/precedent as the `jobs` table and `definition_schedules`,
-- §3.7) — NOT a new source-of-truth exception.
--
-- Denormalization at sync time (Decision 7): when a job carries script_ref, the
-- sync engine copies run_type/command/script/script_path/executor + content_hash
-- from the referenced script ONTO the jobs cache row, so the scheduler, runner
-- polling, execspec, and API serializers keep reading jobs.* unchanged.

CREATE TABLE scripts (
    name         TEXT PRIMARY KEY,
    description  TEXT,
    -- run_type + executor describe the CODE; they move off Job onto Script.
    run_type     TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl')),
    -- Executable source — exactly one of command/script/script_path (validated at
    -- parse time); run_type selects the interpreter when executed.
    command      TEXT,                       -- inline one-liner
    script       TEXT,                       -- inline multi-line body
    script_path  TEXT,                       -- repo-relative file, read from the clone
    executor     TEXT CHECK (executor IN ('runner','ssh')), -- optional default; NULL ⇒ resolve from run_type
    -- Decision 8: 'sha256:'-prefixed hex digest of the resolved body (inline string
    -- or the contents of script_path). Used by run snapshots (§3.3) and migrate dedupe.
    content_hash TEXT NOT NULL,
    source_path  TEXT,                        -- scripts/<name>.yaml in git
    synced_at    TEXT NOT NULL
);

-- Job → Script reference (NEW). NULL ⇒ legacy inline body (back-compat until the
-- Phase 3 migration rewrites all jobs to script_ref). content_hash is the
-- denormalized hash of whatever body the job resolves to (referenced script or
-- inline), so the enqueue path can snapshot it onto the run cheaply (§3.3 / DQ1).
ALTER TABLE jobs ADD COLUMN script_ref   TEXT;
ALTER TABLE jobs ADD COLUMN content_hash TEXT;

-- Run reproducibility (§3.3): snapshot the script identity a run executed, so
-- History stays truthful after a shared script is edited in a later commit. Lives
-- alongside the existing schedule_name + env_json snapshot (migration 090).
-- Per DQ2 the snapshot lives on `runs` only (a workflow_run spans N job runs, each
-- its own job→script, so a single ref on workflow_runs would be ambiguous).
ALTER TABLE runs ADD COLUMN script_ref   TEXT;
ALTER TABLE runs ADD COLUMN content_hash TEXT;

-- Sync bookkeeping: count scripts synced alongside jobs/workflows/scopes.
ALTER TABLE git_sync_events ADD COLUMN scripts_synced INTEGER NOT NULL DEFAULT 0;
