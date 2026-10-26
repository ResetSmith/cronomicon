-- 1700_calendar_catalogue, the way back: the links go, and each copy stays its
-- agency's own calendar, as every calendar was.
DROP TRIGGER IF EXISTS calendars_catalogue_copies_nothing_update;
DROP TRIGGER IF EXISTS calendars_catalogue_copies_nothing_insert;
DROP TRIGGER IF EXISTS calendars_uid_fixed;
DROP TRIGGER IF EXISTS calendars_uid_given;
DROP INDEX IF EXISTS idx_calendars_origin;
DROP INDEX IF EXISTS uq_calendars_uid;
ALTER TABLE calendars DROP COLUMN origin_gone;
ALTER TABLE calendars DROP COLUMN update_mode;
ALTER TABLE calendars DROP COLUMN origin_hash;
ALTER TABLE calendars DROP COLUMN origin_uid;
ALTER TABLE calendars DROP COLUMN uid;
