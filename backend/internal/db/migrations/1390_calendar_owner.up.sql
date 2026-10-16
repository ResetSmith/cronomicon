-- 1390_calendar_owner (2.4.0, §0a, IN-6; SO-1 moved to I1) — a calendar is
-- its agency's. Every calendar there is was written by a global
-- administrator, and is Global's.
--
-- `global = 1` on a calendar means "every entry of its OWN agency" from here:
-- Global's freezes Global, an agency's freezes that agency. The freeze of the
-- whole installation, which Global's global calendar gave until now, is an
-- installation setting (freeze.installation), read wherever that was.
--
-- ADD COLUMN, not a rebuild: calendar_days cascades from calendars, and a
-- rebuild would erase every day (the 830 lesson). The key stays (source,
-- name) until names are per agency (I2, the owner's answer to question 30).

ALTER TABLE calendars ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';
