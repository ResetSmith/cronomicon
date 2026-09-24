-- 1020_satellite_uids — AF-4b stage R2-2: the satellite tables and the nine
-- delete-cascade triggers move onto the definition uid
-- (the rbac2 plan).
--
-- R2-1 moved run HISTORY onto the uid. This moves everything that hangs off a
-- definition while it is alive: its schedule entries, its pause, its parked
-- runs, its reference bindings, its reactions and their delivery log, its file
-- sightings, its log-folder code and its revision history. Behavior-neutral
-- again: names are still globally unique per source, every read path still
-- resolves by name, and the cascades delete exactly the rows they deleted
-- yesterday.
--
-- THE TRIGGERS ARE THE POINT. Nine AFTER DELETE triggers (five on jobs, four on
-- workflows) cascade on OLD.name — and the 940 header is emphatic that a
-- forgotten one "silently stops cascading a delete", leaving orphans the next
-- definition of the same name inherits. Under per-agency naming a name-keyed
-- cascade is worse than forgotten: deleting FIN's 'backup' would take DSS's
-- schedule entries with it. Each trigger is re-keyed here, while names are
-- still unique and the two formulations still agree — which is the only window
-- in which the change can be proven equivalent.
--
-- EACH TRIGGER KEEPS A NAME ARM, deliberately, and it is not belt-and-braces:
-- it is scoped to rows whose owner_uid IS NULL. A satellite row can only carry
-- a NULL uid if some writer failed to stamp it, and that is exactly the case
-- where a uid-only cascade would silently orphan the row — the failure mode
-- this migration exists to prevent, reintroduced by its own fix. While names
-- are unique the name arm is precisely correct; by R2-5 no NULL should remain,
-- and R2-5's rebuild is where the arm comes off.
--
-- NOT DONE HERE, deliberately: the file_watch_sightings de-dupe UNIQUE index
-- stays keyed on (job_source, job_name, path, size, mtime). Swapping a de-dupe
-- constraint onto a nullable column is the one change in this file that could
-- FIRE DUPLICATE RUNS if a row ever carried a NULL uid, and the name key is
-- exactly correct until names may repeat. It moves in R2-5, in the rebuild
-- window, where it is genuinely required. Same reasoning for the entity_codes
-- live-unique index: the uid index is added ALONGSIDE, not instead.

-- ── Columns ─────────────────────────────────────────────────────────────────

ALTER TABLE definition_schedules ADD COLUMN owner_uid    TEXT;
-- The first-class `schedules` row this entry ref-expands (source_ref, migration
-- 190) — a bare name with no source column, so its uid is the only unambiguous
-- handle this table has ever had for it.
ALTER TABLE definition_schedules ADD COLUMN schedule_uid TEXT;
ALTER TABLE paused_jobs          ADD COLUMN owner_uid    TEXT;
ALTER TABLE pending_runs         ADD COLUMN owner_uid    TEXT;
ALTER TABLE reference_bindings   ADD COLUMN owner_uid    TEXT;
ALTER TABLE reactions            ADD COLUMN owner_uid    TEXT;
-- The WATCHED definition. Deliberately not cascaded (880 keeps a reaction whose
-- upstream vanished, so it surfaces as broken rather than being quietly inert),
-- but it still needs an identity to be re-pointed by.
ALTER TABLE reactions            ADD COLUMN on_uid       TEXT;
ALTER TABLE reaction_deliveries  ADD COLUMN owner_uid    TEXT;
ALTER TABLE file_watch_sightings ADD COLUMN job_uid      TEXT;
ALTER TABLE entity_codes         ADD COLUMN uid          TEXT;
ALTER TABLE definition_revisions ADD COLUMN uid          TEXT;

-- ── Backfill ────────────────────────────────────────────────────────────────
--
-- Every one of these tables carries a source column beside its name, so unlike
-- R2-1's activity case the joins are exact and nothing has to decline to guess.
-- A row whose definition is already gone keeps a NULL uid and stays reachable
-- through the triggers' name arm.

UPDATE definition_schedules SET owner_uid = CASE owner_kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = owner_name AND j.source = owner_source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = owner_name AND w.source = owner_source)
END;

-- source_ref names a schedule with no source to qualify it, so it resolves only
-- when the name is unique across the catalog — the R2-1 activity rule.
UPDATE definition_schedules SET schedule_uid =
    (SELECT s.uid FROM schedules s
      WHERE s.name = definition_schedules.source_ref
        AND (SELECT COUNT(*) FROM schedules s2 WHERE s2.name = s.name) = 1)
    WHERE source_ref IS NOT NULL;

UPDATE paused_jobs SET owner_uid = CASE owner_kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = paused_jobs.name AND j.source = paused_jobs.source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = paused_jobs.name AND w.source = paused_jobs.source)
END;

UPDATE pending_runs SET owner_uid = CASE kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = pending_runs.name AND j.source = pending_runs.source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = pending_runs.name AND w.source = pending_runs.source)
END;

-- Only the 'job' owner kind has a uid: a script's identity is still its name
-- (scripts keep PRIMARY KEY (name) and are out of AF-4b's scope by design).
UPDATE reference_bindings SET owner_uid =
    (SELECT j.uid FROM jobs j WHERE j.name = owner_name AND j.source = owner_source)
    WHERE owner_kind = 'job';

UPDATE reactions SET owner_uid = CASE owner_kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = owner_name AND j.source = owner_source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = owner_name AND w.source = owner_source)
END;

UPDATE reactions SET on_uid = CASE on_kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = on_name AND j.source = on_source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = on_name AND w.source = on_source)
END;

UPDATE reaction_deliveries SET owner_uid = CASE owner_kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = owner_name AND j.source = owner_source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = owner_name AND w.source = owner_source)
END;

UPDATE file_watch_sightings SET job_uid =
    (SELECT j.uid FROM jobs j WHERE j.name = job_name AND j.source = job_source);

UPDATE entity_codes SET uid = CASE kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = entity_codes.name AND j.source = entity_codes.source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = entity_codes.name AND w.source = entity_codes.source)
END;

UPDATE definition_revisions SET uid = CASE kind
    WHEN 'job'      THEN (SELECT j.uid FROM jobs      j WHERE j.name = definition_revisions.name AND j.source = definition_revisions.source)
    WHEN 'workflow' THEN (SELECT w.uid FROM workflows w WHERE w.name = definition_revisions.name AND w.source = definition_revisions.source)
    WHEN 'schedule' THEN (SELECT s.uid FROM schedules s WHERE s.name = definition_revisions.name AND s.source = definition_revisions.source)
END;

-- ── Indexes ─────────────────────────────────────────────────────────────────
--
-- The lookups the triggers and the uid-keyed read paths will use. entity_codes
-- gets its live-unique constraint restated in uid terms ALONGSIDE the existing
-- (kind, source, name) one — both hold while names are unique, and keeping the
-- old one is what makes this migration reversible without a rebuild.

CREATE INDEX idx_def_schedules_owner_uid   ON definition_schedules(owner_uid);
CREATE INDEX idx_paused_jobs_owner_uid     ON paused_jobs(owner_uid);
CREATE INDEX idx_pending_runs_owner_uid    ON pending_runs(owner_uid);
CREATE INDEX idx_reference_bindings_owner_uid ON reference_bindings(owner_uid);
CREATE INDEX idx_reactions_owner_uid       ON reactions(owner_uid);
CREATE INDEX idx_reactions_on_uid          ON reactions(on_uid);
CREATE INDEX idx_reaction_deliveries_owner_uid ON reaction_deliveries(owner_uid);
CREATE INDEX idx_file_watch_sightings_uid  ON file_watch_sightings(job_uid, seen_at);
CREATE UNIQUE INDEX idx_entity_codes_live_uid
    ON entity_codes(kind, uid) WHERE deleted_at IS NULL AND uid IS NOT NULL;
CREATE INDEX idx_definition_revisions_uid  ON definition_revisions(uid);

-- ── The nine cascade triggers, re-keyed ─────────────────────────────────────
--
-- Recreated, not rebuilt: these are DROP/CREATE TRIGGER statements, so the
-- tables themselves are untouched and no rowid moves. Each keeps the NULL-uid
-- name arm described in the header.

DROP TRIGGER IF EXISTS trg_def_schedules_job_delete;
CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_def_schedules_workflow_delete;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'workflow'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_paused_jobs_job_delete;
CREATE TRIGGER trg_paused_jobs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_paused_jobs_workflow_delete;
CREATE TRIGGER trg_paused_jobs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'workflow'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_pending_runs_job_delete;
CREATE TRIGGER trg_pending_runs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM pending_runs
    WHERE kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_pending_runs_workflow_delete;
CREATE TRIGGER trg_pending_runs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM pending_runs
    WHERE kind = 'workflow'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND name = OLD.name AND source = OLD.source));
END;

DROP TRIGGER IF EXISTS reference_bindings_job_cleanup;
CREATE TRIGGER reference_bindings_job_cleanup AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'job'
      AND (owner_uid = OLD.uid
        OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_reactions_job_delete;
CREATE TRIGGER trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'job'
       AND (owner_uid = OLD.uid
         OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;

DROP TRIGGER IF EXISTS trg_reactions_workflow_delete;
CREATE TRIGGER trg_reactions_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'workflow'
       AND (owner_uid = OLD.uid
         OR (owner_uid IS NULL AND owner_name = OLD.name AND owner_source = OLD.source));
END;
