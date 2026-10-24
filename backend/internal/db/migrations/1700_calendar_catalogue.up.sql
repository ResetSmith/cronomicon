-- 1700_calendar_catalogue — 2.5.3 CA4a (CA-25 to CA-28): the built-in
-- agency's calendars are the catalogue, and an agency may hold a linked copy
-- of one.
--
-- Every calendar gets a uid, so a copy names the catalogue calendar it came
-- from by identity, not by name: a catalogue calendar deleted and made again
-- under the same name does not re-attach the old copies. It is generated for
-- every row (a trigger, for every writer that does not give one) and never
-- changes.
--
-- A copy is a row with origin_uid set: origin_hash is the content it holds
-- (calendar.ContentHash: description, the two flags, the days and their
-- labels, not the name), update_mode is 'ask' or 'auto', and origin_gone says
-- the catalogue calendar was deleted. A calendar that is no copy has none of
-- them. Nothing is linked here: the copies migration 1430 made stay the
-- agencies' own (CA-25).
--
-- The catalogue copies nothing: a calendar of the built-in agency's may not be
-- a copy.
ALTER TABLE calendars ADD COLUMN uid TEXT;
ALTER TABLE calendars ADD COLUMN origin_uid TEXT;
ALTER TABLE calendars ADD COLUMN origin_hash TEXT;
ALTER TABLE calendars ADD COLUMN update_mode TEXT CHECK (update_mode IS NULL OR update_mode IN ('ask', 'auto'));
ALTER TABLE calendars ADD COLUMN origin_gone INTEGER NOT NULL DEFAULT 0 CHECK (origin_gone IN (0, 1));

UPDATE calendars SET uid = lower(hex(randomblob(16))) WHERE uid IS NULL OR uid = '';
CREATE UNIQUE INDEX uq_calendars_uid ON calendars (uid);
CREATE INDEX idx_calendars_origin ON calendars (origin_uid) WHERE origin_uid IS NOT NULL;

CREATE TRIGGER calendars_uid_given AFTER INSERT ON calendars WHEN NEW.uid IS NULL OR NEW.uid = ''
BEGIN
    UPDATE calendars SET uid = lower(hex(randomblob(16))) WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER calendars_uid_fixed BEFORE UPDATE OF uid ON calendars
  WHEN OLD.uid IS NOT NULL AND OLD.uid <> '' AND NEW.uid IS NOT OLD.uid
BEGIN
    SELECT RAISE(ABORT, 'calendar_uid_fixed: a calendar''s uid never changes');
END;

CREATE TRIGGER calendars_catalogue_copies_nothing_insert BEFORE INSERT ON calendars
  WHEN NEW.owner_agency = 'global' AND NEW.origin_uid IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'catalogue_copies_nothing: a calendar of the built-in agency''s is the catalogue, never a copy');
END;

CREATE TRIGGER calendars_catalogue_copies_nothing_update BEFORE UPDATE OF owner_agency, origin_uid ON calendars
  WHEN NEW.owner_agency = 'global' AND NEW.origin_uid IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'catalogue_copies_nothing: a calendar of the built-in agency''s is the catalogue, never a copy');
END;
