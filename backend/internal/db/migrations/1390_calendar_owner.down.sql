-- 1390_calendar_owner (down) — calendars have no owner again; the
-- installation freeze setting is left in place, read by nothing.

ALTER TABLE calendars DROP COLUMN owner_agency;
