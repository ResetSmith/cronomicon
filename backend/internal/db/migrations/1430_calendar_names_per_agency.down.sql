-- 1430_calendar_names_per_agency (down) — a calendar's name is unique across
-- the installation again. The copies the upgrade made go first; fails, and
-- changes nothing, if two agencies have calendars of one name by then.

DELETE FROM calendars WHERE created_by LIKE 'upgrade 2.4.0%';

CREATE TABLE _1430_days AS
SELECT calendar_source, calendar_name, day, label, rule FROM calendar_days;
DROP TABLE calendar_days;
DROP INDEX IF EXISTS uq_calendars_owner_name;

CREATE TABLE calendars_new (
    source            TEXT NOT NULL DEFAULT 'cronomicon',
    name              TEXT NOT NULL,
    description       TEXT,
    global            INTEGER NOT NULL DEFAULT 0 CHECK (global IN (0, 1)),
    record_suppressed INTEGER NOT NULL DEFAULT 0 CHECK (record_suppressed IN (0, 1)),
    created_by        TEXT,
    created_at        TEXT NOT NULL,
    last_modified_by  TEXT,
    last_modified_at  TEXT,
    owner_agency      TEXT NOT NULL DEFAULT 'global',
    PRIMARY KEY (source, name)
);
INSERT INTO calendars_new (source, name, description, global, record_suppressed, created_by, created_at,
                           last_modified_by, last_modified_at, owner_agency)
SELECT source, name, description, global, record_suppressed, created_by, created_at,
       last_modified_by, last_modified_at, owner_agency
  FROM calendars;
DROP TABLE calendars;
ALTER TABLE calendars_new RENAME TO calendars;

CREATE TABLE calendar_days (
    calendar_source TEXT NOT NULL,
    calendar_name   TEXT NOT NULL,
    day             TEXT NOT NULL,
    label           TEXT,
    rule            TEXT,
    PRIMARY KEY (calendar_source, calendar_name, day),
    FOREIGN KEY (calendar_source, calendar_name)
        REFERENCES calendars (source, name) ON DELETE CASCADE
);
CREATE INDEX idx_calendar_days_day ON calendar_days(day);
INSERT INTO calendar_days (calendar_source, calendar_name, day, label, rule)
SELECT calendar_source, calendar_name, day, label, rule FROM _1430_days;
DROP TABLE _1430_days;
