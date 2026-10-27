-- 1710_definition_calendars, the way back: a definition's own calendars go.
ALTER TABLE workflows DROP COLUMN allow_calendars;
ALTER TABLE workflows DROP COLUMN forbid_calendars;
ALTER TABLE jobs DROP COLUMN allow_calendars;
ALTER TABLE jobs DROP COLUMN forbid_calendars;
