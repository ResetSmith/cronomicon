-- Reverse 870.
--
-- Dropping the bindings returns every entry to "fires whenever its schedule
-- matches" — a holiday-skipping job resumes running on holidays, and an
-- only-days job resumes running every day. That is the only sound reading once
-- the bindings are gone, and it is the same shape as 770's rollback note for
-- activation windows.
--
-- ⚠️ The provenance column goes with them, so previously recorded suppressions
-- keep their `skipped` status and their human-readable queued_reason but become
-- unfilterable by calendar. The audit rows survive; only the structured query
-- does not.
DROP INDEX IF EXISTS idx_workflow_runs_suppressed_by_calendar;
DROP INDEX IF EXISTS idx_runs_suppressed_by_calendar;

ALTER TABLE workflow_runs  DROP COLUMN queued_reason;
ALTER TABLE workflow_runs  DROP COLUMN suppressed_by_calendar;
ALTER TABLE runs           DROP COLUMN suppressed_by_calendar;

ALTER TABLE definition_schedules DROP COLUMN only_calendars;
ALTER TABLE definition_schedules DROP COLUMN skip_calendars;

ALTER TABLE schedules            DROP COLUMN only_calendars;
ALTER TABLE schedules            DROP COLUMN skip_calendars;
