-- Reverse 860. Dropping the calendars takes their days with them (the cascade
-- does the work, but the explicit order is kept so the intent is readable and so
-- the rollback does not depend on FK enforcement being live).
--
-- Any schedule entry still NAMING a dropped calendar is left with a dangling
-- reference, whose behaviour §2.2 defines by set arithmetic rather than by
-- special case: a `skip` binding contributes the empty set and the entry FIRES
-- (a visible, correctable policy violation), an `only` binding contributes no
-- allowed days and the entry NEVER FIRES (silence, which is what "only run on
-- these days" asked for). Migration 870's down removes those bindings, so a full
-- rollback of both leaves no dangling names behind.
DROP INDEX IF EXISTS idx_calendar_days_day;
DROP TABLE IF EXISTS calendar_days;
DROP TABLE IF EXISTS calendars;
