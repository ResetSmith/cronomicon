-- Reverse AW-1. Dropping the window columns returns every entry to
-- "fires whenever its cron matches" — any deferred or expired entry becomes
-- immediately active, which is the only sound interpretation once the bounds
-- are gone.
ALTER TABLE definition_schedules DROP COLUMN end_at;
ALTER TABLE definition_schedules DROP COLUMN start_at;

ALTER TABLE schedules DROP COLUMN end_at;
ALTER TABLE schedules DROP COLUMN start_at;
