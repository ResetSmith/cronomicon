-- 920_definition_revisions — revision history + recycle bin for cronomicon-source
-- definitions (RH, the prod-features plan §4).
--
-- WHY. Git-source definitions get history, diff, blame and restore from Git.
-- In-app (cronomicon-source) Jobs, Workflows and Schedules got none of it: rows
-- were edited in place and deleted hard, and the config-change audit log records
-- THAT a change happened, never a restorable snapshot of WHAT. As the dual-source
-- model pushes more authoring in-app, "an admin fat-fingered the workflow editor"
-- goes from theoretical to a Tuesday.
--
-- PURELY ADDITIVE — no table is rebuilt. Verified before writing: no table in the
-- schema carries an inbound FK to jobs, workflows or schedules (runs references
-- jobs by (job_name, job_source) TEXT with no constraint, deliberately — see
-- 710's header), so `deleted_at` is a plain ALTER on each. That means none of the
-- 490 rebuild checklist and none of 830/890's FK stash-and-restore applies here.
--
-- ⚠️ THE SOFT DELETE IS AN UPDATE, SO THE DELETE TRIGGERS DO NOT FIRE.
-- trg_def_schedules_job_delete / trg_paused_jobs_job_delete and their workflow
-- twins (migrations 200/490) are AFTER DELETE. A binned definition therefore
-- KEEPS its definition_schedules and paused_jobs rows, and the scheduler's
-- reload query would happily keep firing it. The fix is deliberately at the READ
-- side — every execution path filters `deleted_at IS NULL` — rather than deleting
-- the bindings on the way in, because bindings destroyed at delete time cannot be
-- brought back by an undelete. Restoring a definition must be one UPDATE that
-- loses nothing.
--
-- THE NAME STAYS TAKEN. jobs/workflows/schedules are keyed PRIMARY KEY
-- (source, name), so a binned row still occupies its name and creating a
-- replacement 409s until the original is restored or purged. That is accepted
-- rather than worked around: rename-on-delete would break the "restore is one
-- UPDATE" property above, and the 409 names the recycle bin so the operator
-- knows which lever to pull.

ALTER TABLE jobs ADD COLUMN deleted_at TEXT;
ALTER TABLE jobs ADD COLUMN deleted_by TEXT;
ALTER TABLE workflows ADD COLUMN deleted_at TEXT;
ALTER TABLE workflows ADD COLUMN deleted_by TEXT;
ALTER TABLE schedules ADD COLUMN deleted_at TEXT;
ALTER TABLE schedules ADD COLUMN deleted_by TEXT;

-- Partial indexes: the recycle-bin listing and the retention purge both scan
-- only the dead, which are a rounding error next to the live rows.
CREATE INDEX idx_jobs_deleted ON jobs(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX idx_workflows_deleted ON workflows(deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX idx_schedules_deleted ON schedules(deleted_at) WHERE deleted_at IS NOT NULL;

-- The append-only snapshot log.
--
-- SNAPSHOT SHAPE: the COMPOSE INPUT, not the DB row. There is no canonical
-- serialization of a definition anywhere in the tree — no YAML marshaller exists
-- (the git path is parse-only), jobs.content_hash is the referenced SCRIPT's
-- hash rather than the definition's, and workflows have no hash column at all.
-- The compose input is what an operator actually authored, it is already the
-- exact shape the write path validates, and restoring is therefore
-- re-submitting it through that same path — so validation, RBAC, entity-code
-- allocation and the audit trail all still happen. No special-case DB surgery,
-- and no second definition of "what a job is" to keep in sync.
CREATE TABLE definition_revisions (
    id             TEXT PRIMARY KEY,        -- UUIDv7
    kind           TEXT NOT NULL CHECK (kind IN ('job','workflow','schedule')),
    source         TEXT NOT NULL,           -- always 'cronomicon' today; git history is Git's
    name           TEXT NOT NULL,
    revision_no    INTEGER NOT NULL,        -- 1-based, monotonic per (kind, source, name)
    action         TEXT NOT NULL CHECK (action IN ('created','updated','deleted','restored')),
    actor          TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    snapshot_json  TEXT NOT NULL,
    -- sha256 of snapshot_json. Lets a no-op save (open the editor, press Save,
    -- change nothing) skip writing a revision, so the history reads as a list of
    -- actual changes rather than of button presses.
    snapshot_digest TEXT NOT NULL
);

-- One row per revision number per definition; also the ordering index the
-- history list reads.
CREATE UNIQUE INDEX uq_definition_revisions
    ON definition_revisions(kind, source, name, revision_no);

-- The retention reaper's scan.
CREATE INDEX idx_definition_revisions_age ON definition_revisions(created_at);
