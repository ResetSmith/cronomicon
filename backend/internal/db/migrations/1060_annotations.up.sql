-- 1060_annotations — AN-1: operator-owned notes, criticality and contact for
-- jobs and workflows (the annotations plan).
--
-- Replaces the role Automate's free-text "note" plays: whether a definition is
-- critical, who to call when it breaks, and what it is meant to accomplish.
-- Distinct from the git `description`, which stays git-owned and
-- sync-overwritten — these are operator-owned and sync-PRESERVED, exactly like
-- tags (tags-support.md D6). The two coexist; neither writes the other.
--
-- ONE SIDECAR TABLE, not columns on jobs/workflows. Two reasons, both learned
-- the hard way: one table serves both owner kinds without duplicating the
-- shape, and it stays out of the R2-style table rebuilds whose
-- `INSERT … SELECT` column lists were hand-carried five times across 1000–1050.
--
-- NO FOREIGN KEY. The parent differs per row (jobs or workflows), which no
-- single FK can express — the same reason reference_bindings (590) and
-- paused_jobs (170) have none. Lifecycle is triggers plus the sync sweep;
-- see the trigger note below for what that costs.
CREATE TABLE annotations (
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    -- The UID, never the name. Under R2-5 two cronomicon definitions may share a
    -- name within a source, so a name-keyed sidecar would hand one twin the
    -- other's notes — the exact defect class R2F-1 just fixed for bindings.
    -- There is deliberately no name column to fall back to.
    owner_uid  TEXT NOT NULL,
    -- DISPLAY-ONLY in this band (AN-Q4): colours a chip and adds a line to a
    -- notification body. It routes nothing, schedules nothing and pages nobody
    -- differently. Giving it routing meaning later is a semantic contract that
    -- needs a fleet audit first.
    critical   INTEGER NOT NULL DEFAULT 0 CHECK (critical IN (0,1)),
    -- Free text, not a user/agency reference: the real answer is usually a
    -- distribution list or an on-call rotation that is not an Cronomicon user, so
    -- a foreign key here would break on first contact.
    contact    TEXT NOT NULL DEFAULT '',
    notes      TEXT NOT NULL DEFAULT '',
    -- Last-writer-wins with attribution, so staleness is visible. This is not a
    -- comment thread; running commentary would be a different table.
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (owner_kind, owner_uid)
);

-- Hard delete cascades. AFTER DELETE, keyed on OLD.uid with NO name arm — the
-- R2F-1 STATUS lesson: a name arm on a uid-keyed satellite stops being "merely
-- redundant" the moment names duplicate and starts deleting a sibling's rows.
--
-- ⚠️ These triggers live ON jobs and ON workflows, not on `annotations`. SQLite
-- drops a trigger with its own table only, so any future migration that
-- rebuilds jobs or workflows the 1050 way (CREATE _new / INSERT SELECT / DROP /
-- RENAME) silently drops both and the cascade stops without erroring. Recreate
-- them in any such migration; TestAnnotationCascadeOnBareDelete is the guard.
CREATE TRIGGER annotations_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM annotations WHERE owner_kind = 'job' AND owner_uid = OLD.uid;
END;

CREATE TRIGGER annotations_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM annotations WHERE owner_kind = 'workflow' AND owner_uid = OLD.uid;
END;

-- Soft delete (the recycle bin) is an UPDATE of deleted_at, not a DELETE, so
-- these triggers do not fire and a binned definition KEEPS its annotation —
-- restore round-trips it for free. That is intended, and pinned by
-- TestAnnotationSurvivesSoftDeleteRestore rather than left to inference.
