-- 1300_schedule_identity (down) — a schedule is unique by (source, name) again.
--
-- Lossy only where it must be: if two agencies hold a schedule of one name in
-- one source, only one can keep it. Global's is kept, else the oldest row. The
-- entries of definitions bound to a dropped schedule keep their timing and
-- lose the link (schedule_uid is cleared), as an entry of a deleted schedule
-- always has.

DROP INDEX IF EXISTS idx_def_schedules_schedule_uid;

UPDATE definition_schedules SET schedule_uid = NULL
 WHERE schedule_uid IN (
       SELECT s.uid FROM schedules s
        WHERE s.owner_agency != 'global'
          AND EXISTS (SELECT 1 FROM schedules g
                       WHERE g.source = s.source AND g.name = s.name AND g.owner_agency = 'global'));
DELETE FROM schedules
 WHERE owner_agency != 'global'
   AND EXISTS (SELECT 1 FROM schedules g
                WHERE g.source = schedules.source AND g.name = schedules.name AND g.owner_agency = 'global');
UPDATE definition_schedules SET schedule_uid = NULL
 WHERE schedule_uid IN (
       SELECT s.uid FROM schedules s
        WHERE s.rowid NOT IN (SELECT MIN(rowid) FROM schedules GROUP BY source, name));
DELETE FROM schedules WHERE rowid NOT IN (SELECT MIN(rowid) FROM schedules GROUP BY source, name);

CREATE TABLE schedules_old (
    uid              TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    description      TEXT,
    cron             TEXT NOT NULL,
    env              TEXT,
    content_hash     TEXT NOT NULL,
    source_path      TEXT,
    synced_at        TEXT,
    created_by       TEXT,
    created_at       TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    start_at         TEXT,
    end_at           TEXT,
    interval         TEXT,
    skip_calendars   TEXT,
    only_calendars   TEXT,
    deleted_at       TEXT,
    deleted_by       TEXT,
    UNIQUE (source, name)
);
INSERT INTO schedules_old SELECT
    uid, name, source, description, cron, env, content_hash, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, start_at, end_at, interval, skip_calendars, only_calendars,
    deleted_at, deleted_by
FROM schedules;
DROP TABLE schedules;
ALTER TABLE schedules_old RENAME TO schedules;

CREATE INDEX idx_schedules_deleted ON schedules(deleted_at) WHERE deleted_at IS NOT NULL;
