-- Reverse 1020.
--
-- The nine triggers are restored to their pre-1020 name-keyed form FIRST, so a
-- database rolled back here still cascades deletes correctly the moment the
-- columns those triggers reference disappear. Getting this order wrong would
-- leave triggers referring to dropped columns, and SQLite only discovers that
-- when the trigger fires — i.e. during a delete, silently, at the worst moment.
--
-- Dropping the columns is lossy only in the derived sense: every uid here is
-- recomputable from the row's own (source, name) plus the definition tables,
-- which is precisely what the up-migration's backfill does.

DROP TRIGGER IF EXISTS trg_def_schedules_job_delete;
CREATE TRIGGER trg_def_schedules_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'job' AND owner_name = OLD.name AND owner_source = OLD.source;
END;

DROP TRIGGER IF EXISTS trg_def_schedules_workflow_delete;
CREATE TRIGGER trg_def_schedules_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM definition_schedules
    WHERE owner_kind = 'workflow' AND owner_name = OLD.name AND owner_source = OLD.source;
END;

DROP TRIGGER IF EXISTS trg_paused_jobs_job_delete;
CREATE TRIGGER trg_paused_jobs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'job' AND name = OLD.name AND source = OLD.source;
END;

DROP TRIGGER IF EXISTS trg_paused_jobs_workflow_delete;
CREATE TRIGGER trg_paused_jobs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'workflow' AND name = OLD.name AND source = OLD.source;
END;

DROP TRIGGER IF EXISTS trg_pending_runs_job_delete;
CREATE TRIGGER trg_pending_runs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM pending_runs WHERE kind = 'job' AND source = OLD.source AND name = OLD.name;
END;

DROP TRIGGER IF EXISTS trg_pending_runs_workflow_delete;
CREATE TRIGGER trg_pending_runs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM pending_runs WHERE kind = 'workflow' AND source = OLD.source AND name = OLD.name;
END;

DROP TRIGGER IF EXISTS reference_bindings_job_cleanup;
CREATE TRIGGER reference_bindings_job_cleanup AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

DROP TRIGGER IF EXISTS trg_reactions_job_delete;
CREATE TRIGGER trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

DROP TRIGGER IF EXISTS trg_reactions_workflow_delete;
CREATE TRIGGER trg_reactions_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'workflow' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

DROP INDEX IF EXISTS idx_def_schedules_owner_uid;
DROP INDEX IF EXISTS idx_paused_jobs_owner_uid;
DROP INDEX IF EXISTS idx_pending_runs_owner_uid;
DROP INDEX IF EXISTS idx_reference_bindings_owner_uid;
DROP INDEX IF EXISTS idx_reactions_owner_uid;
DROP INDEX IF EXISTS idx_reactions_on_uid;
DROP INDEX IF EXISTS idx_reaction_deliveries_owner_uid;
DROP INDEX IF EXISTS idx_file_watch_sightings_uid;
DROP INDEX IF EXISTS idx_entity_codes_live_uid;
DROP INDEX IF EXISTS idx_definition_revisions_uid;

ALTER TABLE definition_schedules DROP COLUMN owner_uid;
ALTER TABLE definition_schedules DROP COLUMN schedule_uid;
ALTER TABLE paused_jobs          DROP COLUMN owner_uid;
ALTER TABLE pending_runs         DROP COLUMN owner_uid;
ALTER TABLE reference_bindings   DROP COLUMN owner_uid;
ALTER TABLE reactions            DROP COLUMN owner_uid;
ALTER TABLE reactions            DROP COLUMN on_uid;
ALTER TABLE reaction_deliveries  DROP COLUMN owner_uid;
ALTER TABLE file_watch_sightings DROP COLUMN job_uid;
ALTER TABLE entity_codes         DROP COLUMN uid;
ALTER TABLE definition_revisions DROP COLUMN uid;
