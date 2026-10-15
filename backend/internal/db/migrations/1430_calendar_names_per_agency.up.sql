-- 1430_calendar_names_per_agency — a calendar's name is unique within its
-- agency, and each agency gets its own copies (2.4.0, §0a, IN-5, IN-6; Phase
-- I2; the owner's answers to questions 30 and 35, and the choice of
-- 2026-10-14, "live until I2, then copies").
--
-- A calendar has had an owner since 1390, and its name stayed unique across
-- the installation (PRIMARY KEY (source, name)): one agency's `holidays` kept
-- every other from having one, and an agency's entry that named Global's
-- calendar went on following it, live, as the owner chose, until this.
--
-- The key is now (owner_agency, source, name), and a name is held once per
-- agency whatever its source. calendar_days references its calendar by owner
-- too. Both tables are rebuilt: the days are copied aside first and put back
-- after, because DROP TABLE performs an implicit DELETE that fires the ON
-- DELETE CASCADE while foreign keys are on (the 830 lesson, which 860 and 1390
-- record).
--
-- Then the copies, under the same name, so that no binding changes:
--   * every agency besides Global gets a copy of each of Global's calendars
--     that is global (question 35): until 1390 such a calendar froze every
--     agency's fires, and an agency that was frozen by it, and named it
--     nowhere, keeps its freeze days this way;
--   * every agency whose definition's entry, or whose schedule, names a
--     calendar it has none of gets a copy of that calendar (Global's where
--     Global has one, else the first by agency): the binding it kept live
--     since 1390 now names its own.
-- A copy is the agency's own from here (created_by says where it came from;
-- C10's "an update is available" is for a copy made by an assignment). The
-- notice calendar_other_agency, which listed the live bindings, has nothing
-- left to list and goes.

CREATE TABLE _1430_days AS
SELECT c.owner_agency AS calendar_owner, d.calendar_source, d.calendar_name, d.day, d.label, d.rule
  FROM calendar_days d
  JOIN calendars c ON c.source = d.calendar_source AND c.name = d.calendar_name;

DROP TABLE calendar_days;

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
    PRIMARY KEY (owner_agency, source, name)
);
INSERT INTO calendars_new (source, name, description, global, record_suppressed, created_by, created_at,
                           last_modified_by, last_modified_at, owner_agency)
SELECT source, name, description, global, record_suppressed, created_by, created_at,
       last_modified_by, last_modified_at, owner_agency
  FROM calendars;
DROP TABLE calendars;
ALTER TABLE calendars_new RENAME TO calendars;
-- A name once per agency, whatever the source (the app asked that of the
-- whole installation until now).
CREATE UNIQUE INDEX uq_calendars_owner_name ON calendars (owner_agency, name);

CREATE TABLE calendar_days (
    calendar_owner  TEXT NOT NULL,
    calendar_source TEXT NOT NULL,
    calendar_name   TEXT NOT NULL,
    day             TEXT NOT NULL,
    label           TEXT,
    rule            TEXT,
    PRIMARY KEY (calendar_owner, calendar_source, calendar_name, day),
    FOREIGN KEY (calendar_owner, calendar_source, calendar_name)
        REFERENCES calendars (owner_agency, source, name) ON DELETE CASCADE
);
CREATE INDEX idx_calendar_days_day ON calendar_days(day);
INSERT INTO calendar_days (calendar_owner, calendar_source, calendar_name, day, label, rule)
SELECT calendar_owner, calendar_source, calendar_name, day, label, rule FROM _1430_days;
DROP TABLE _1430_days;

-- The copies to make: (agency, calendar name, the calendar copied from).
CREATE TABLE _1430_copies (agency TEXT NOT NULL, name TEXT NOT NULL, from_owner TEXT NOT NULL, from_source TEXT NOT NULL,
                           PRIMARY KEY (agency, name));

-- (a) Global's global calendars, to every agency (question 35).
INSERT OR IGNORE INTO _1430_copies (agency, name, from_owner, from_source)
SELECT a.id, c.name, c.owner_agency, c.source
  FROM calendars c JOIN agencies a ON a.id <> 'global'
 WHERE c.owner_agency = 'global' AND c.global = 1;

-- (b) What an agency's bindings name and the agency has none of. The agency
-- of a definition's entry is its definition's (a job's owner, 1400; a Git
-- job's repository's; a workflow's owner, 1360); a schedule's is its own.
CREATE TABLE _1430_bound (agency TEXT NOT NULL, cal TEXT NOT NULL, PRIMARY KEY (agency, cal));
INSERT OR IGNORE INTO _1430_bound (agency, cal)
SELECT b.agency, je.value
  FROM (
    SELECT COALESCE(CASE ds.owner_kind
             WHEN 'job' THEN (SELECT CASE WHEN j.source = 'git' AND j.repo_id IS NOT NULL
                                          THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = j.repo_id)
                                          ELSE j.owner_agency END
                                FROM jobs j WHERE j.uid = ds.owner_uid)
             ELSE (SELECT CASE WHEN w.source = 'git'
                               THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                               ELSE w.owner_agency END
                     FROM workflows w WHERE w.uid = ds.owner_uid)
           END, 'global') AS agency,
           COALESCE(ds.skip_calendars, '[]') AS skip, COALESCE(ds.only_calendars, '[]') AS only
      FROM definition_schedules ds
     WHERE COALESCE(ds.owner_uid, '') <> ''
    UNION ALL
    SELECT s.owner_agency, COALESCE(s.skip_calendars, '[]'), COALESCE(s.only_calendars, '[]')
      FROM schedules s WHERE s.deleted_at IS NULL
  ) b, json_each(b.skip) je
 WHERE json_valid(b.skip) AND je.value <> '';
INSERT OR IGNORE INTO _1430_bound (agency, cal)
SELECT b.agency, je.value
  FROM (
    SELECT COALESCE(CASE ds.owner_kind
             WHEN 'job' THEN (SELECT CASE WHEN j.source = 'git' AND j.repo_id IS NOT NULL
                                          THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = j.repo_id)
                                          ELSE j.owner_agency END
                                FROM jobs j WHERE j.uid = ds.owner_uid)
             ELSE (SELECT CASE WHEN w.source = 'git'
                               THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
                               ELSE w.owner_agency END
                     FROM workflows w WHERE w.uid = ds.owner_uid)
           END, 'global') AS agency,
           COALESCE(ds.only_calendars, '[]') AS only
      FROM definition_schedules ds
     WHERE COALESCE(ds.owner_uid, '') <> ''
    UNION ALL
    SELECT s.owner_agency, COALESCE(s.only_calendars, '[]')
      FROM schedules s WHERE s.deleted_at IS NULL
  ) b, json_each(b.only) je
 WHERE json_valid(b.only) AND je.value <> '';

INSERT OR IGNORE INTO _1430_copies (agency, name, from_owner, from_source)
SELECT bd.agency, bd.cal, c.owner_agency, c.source
  FROM _1430_bound bd
  JOIN calendars c ON c.name = bd.cal
   AND c.owner_agency = COALESCE((SELECT x.owner_agency FROM calendars x WHERE x.name = bd.cal AND x.owner_agency = 'global'),
                                 (SELECT MIN(x.owner_agency) FROM calendars x WHERE x.name = bd.cal))
 WHERE NOT EXISTS (SELECT 1 FROM calendars own WHERE own.owner_agency = bd.agency AND own.name = bd.cal)
   AND EXISTS (SELECT 1 FROM agencies a WHERE a.id = bd.agency);

INSERT OR IGNORE INTO calendars (source, name, description, global, record_suppressed, created_by, created_at,
                                 last_modified_by, last_modified_at, owner_agency)
SELECT 'cronomicon', c.name, c.description, c.global, c.record_suppressed,
       'upgrade 2.4.0 (a copy of ' || (SELECT COALESCE(a.name, c.owner_agency) FROM agencies a WHERE a.id = c.owner_agency) || '''s)',
       c.created_at, c.last_modified_by, c.last_modified_at, cp.agency
  FROM _1430_copies cp
  JOIN calendars c ON c.owner_agency = cp.from_owner AND c.source = cp.from_source AND c.name = cp.name
 WHERE NOT EXISTS (SELECT 1 FROM calendars own WHERE own.owner_agency = cp.agency AND own.name = cp.name);

INSERT OR IGNORE INTO calendar_days (calendar_owner, calendar_source, calendar_name, day, label, rule)
SELECT cp.agency, 'cronomicon', cp.name, d.day, d.label, d.rule
  FROM _1430_copies cp
  JOIN calendar_days d ON d.calendar_owner = cp.from_owner AND d.calendar_source = cp.from_source AND d.calendar_name = cp.name
  JOIN calendars own ON own.owner_agency = cp.agency AND own.source = 'cronomicon' AND own.name = cp.name
 WHERE own.created_by LIKE 'upgrade 2.4.0%';

DROP TABLE _1430_bound;
DROP TABLE _1430_copies;

-- Nothing is left for it to list: every binding names its own agency's now.
DELETE FROM notices WHERE kind = 'calendar_other_agency';
