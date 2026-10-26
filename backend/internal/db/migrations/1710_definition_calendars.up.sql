-- 1710_definition_calendars — 2.5.3 CA4b (CA-30): a job and a workflow may name
-- calendars to Forbid (do not run on these days) and to Allow (run only on
-- these days), from their own agency's calendars, as a schedule entry names
-- skip and only calendars (870). JSON arrays of names, NULL for none.
--
-- The rows the authors write are the rows the scheduler reads, so the columns
-- go on jobs and workflows alone; nothing is copied down to entries (a
-- definition's calendars gate every automatic start, not only its schedule's,
-- CA-32).
ALTER TABLE jobs ADD COLUMN forbid_calendars TEXT;
ALTER TABLE jobs ADD COLUMN allow_calendars TEXT;
ALTER TABLE workflows ADD COLUMN forbid_calendars TEXT;
ALTER TABLE workflows ADD COLUMN allow_calendars TEXT;
