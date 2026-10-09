-- 1300_schedule_identity — a schedule belongs to an agency, and a definition
-- knows WHICH schedule it took its timing from (2.4.0, GR-6 and GR-8).
--
-- A reusable schedule has had a uid since 1010 and has been unique by
-- (source, name) since 160: one `nightly` from Git, one built in the app. A
-- definition bound to one holds a COPY of its timing in definition_schedules,
-- with the schedule's NAME in source_ref, and until now every statement that
-- went from a schedule to the definitions bound to it went by that name. So
-- the in-app `nightly` and Git's `nightly` shared their users: editing one
-- rewrote the timing of the jobs bound to the other, and deleting one detached
-- them (H12 of the 2.5.0 plan). `schedule_uid` existed (1020) but was filled
-- only when the name happened to be unique, and exactly one statement read it.
--
-- 2.4.0 gives each agency a repository, so there will be a `nightly` per
-- agency as well. This migration makes the two things true that the rest of
-- the release stands on, and changes nothing about how many repositories or
-- owners there are: every schedule is Global's today.
--
--   * `schedules.owner_agency`: the agency the schedule belongs to. A Git
--     schedule's is its repository's; an in-app one's is chosen by its author
--     (the picker arrives with the screens). Never empty. The name is unique
--     per (source, owner_agency), no longer per source.
--   * `schedules.repo_id`: the repository a Git schedule came from (GR-3).
--     NULL for a schedule built in the app, which has none.
--   * `definition_schedules.schedule_uid` is filled for every entry that was
--     expanded from a schedule and can be attributed to ONE schedule, and is
--     indexed. Code keys on it from here on; source_ref stays, as the name the
--     definition's author wrote.

CREATE TABLE schedules_new (
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
    owner_agency     TEXT NOT NULL DEFAULT 'global',
    repo_id          TEXT,
    UNIQUE (source, owner_agency, name)
);

INSERT INTO schedules_new SELECT
    uid, name, source, description, cron, env, content_hash, source_path,
    synced_at, created_by, created_at, last_modified_by, last_modified_at,
    tags, start_at, end_at, interval, skip_calendars, only_calendars,
    deleted_at, deleted_by,
    'global',
    CASE WHEN source = 'git' THEN 'global' END
FROM schedules;

DROP TABLE schedules;
ALTER TABLE schedules_new RENAME TO schedules;

CREATE INDEX idx_schedules_deleted ON schedules(deleted_at) WHERE deleted_at IS NOT NULL;

-- ── definition_schedules: the leftovers of a restore ────────────────────────
--
-- Restoring a schedule from the recycle bin put its bindings back with neither
-- the owner's uid nor the schedule's. Nothing that keys on a uid saw such a
-- row, so saving the owning definition again added a second entry of the same
-- name instead of replacing it. Those rows are folded back in here, so that
-- every entry of a definition that still exists carries its owner's uid.
--
-- All three steps act only where (source, name) is exactly ONE definition.
-- Two agencies may each own an in-app job of one name, and a uid-less row
-- under that name may be either's: it is left exactly as it is (it fires
-- through the scheduler's name arm, as it did), because folding it in would
-- mean deciding whose it was, and deleting it as "the stale copy" of the one
-- that happens to have a uid'd twin would take the other's timing away.
--
-- First the duplicates: an entry with no owner uid that has a twin (the same
-- owner, the same entry name) WITH one is the stale copy; and of several
-- uid-less twins, one is kept.
DELETE FROM definition_schedules
 WHERE owner_uid IS NULL
   AND CASE owner_kind
         WHEN 'job'      THEN (SELECT COUNT(*) FROM jobs j
                                WHERE j.source = definition_schedules.owner_source AND j.name = definition_schedules.owner_name)
         WHEN 'workflow' THEN (SELECT COUNT(*) FROM workflows w
                                WHERE w.source = definition_schedules.owner_source AND w.name = definition_schedules.owner_name)
       END = 1
   AND EXISTS (SELECT 1 FROM definition_schedules d2
                WHERE d2.owner_uid IS NOT NULL
                  AND d2.owner_kind = definition_schedules.owner_kind
                  AND d2.owner_source = definition_schedules.owner_source
                  AND d2.owner_name = definition_schedules.owner_name
                  AND d2.name = definition_schedules.name);
DELETE FROM definition_schedules
 WHERE owner_uid IS NULL
   AND CASE owner_kind
         WHEN 'job'      THEN (SELECT COUNT(*) FROM jobs j
                                WHERE j.source = definition_schedules.owner_source AND j.name = definition_schedules.owner_name)
         WHEN 'workflow' THEN (SELECT COUNT(*) FROM workflows w
                                WHERE w.source = definition_schedules.owner_source AND w.name = definition_schedules.owner_name)
       END = 1
   AND rowid NOT IN (SELECT MIN(rowid) FROM definition_schedules
                      WHERE owner_uid IS NULL
                      GROUP BY owner_kind, owner_source, owner_name, name);
-- Then the owner: stamped when (source, name) is exactly one definition. Under
-- two same-named in-app definitions it stays NULL: which one a uid-less row
-- belonged to cannot be known, and a guess would hand one agency's timing to
-- another's job.
UPDATE definition_schedules
   SET owner_uid = CASE owner_kind
         WHEN 'job'      THEN (SELECT CASE WHEN COUNT(*) = 1 THEN MAX(j.uid) END FROM jobs j
                                WHERE j.source = definition_schedules.owner_source AND j.name = definition_schedules.owner_name)
         WHEN 'workflow' THEN (SELECT CASE WHEN COUNT(*) = 1 THEN MAX(w.uid) END FROM workflows w
                                WHERE w.source = definition_schedules.owner_source AND w.name = definition_schedules.owner_name)
       END
 WHERE owner_uid IS NULL;

-- ── definition_schedules: which schedule ────────────────────────────────────
--
-- An entry that already carries the uid of a schedule that EXISTS keeps it: it
-- was stamped when the name was unique, by the writer that expanded it. The
-- rest are entries written while two schedules shared the name, and entries
-- whose uid names a schedule that has since gone (a Git schedule removed from
-- the repository leaves the in-app definitions bound to it with its uid).
-- Until now such an entry was found by its NAME by whichever schedule came to
-- hold that name; a dead uid would hide it from every schedule for good, so it
-- is cleared here and attributed by the same rules as the rest.
UPDATE definition_schedules
   SET schedule_uid = NULL
 WHERE schedule_uid IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM schedules s WHERE s.uid = definition_schedules.schedule_uid);
--
-- An entry of a GIT definition was expanded by sync, and sync resolves a
-- scheduleRef only among the schedules of the repository it is syncing: it is
-- the Git schedule's.
UPDATE definition_schedules
   SET schedule_uid = (SELECT s.uid FROM schedules s
                        WHERE s.source = 'git' AND s.owner_agency = 'global'
                          AND s.name = definition_schedules.source_ref)
 WHERE schedule_uid IS NULL AND source_ref IS NOT NULL AND source_ref != ''
   AND owner_source = 'git';
-- An entry of an IN-APP definition was expanded from whichever schedule of
-- that name the composer found first when the definition was last saved. If
-- one schedule holds the name now, it is that one. If two do, it cannot be
-- known which, and the entry is left without a uid: it goes on firing with the
-- timing it has, follows neither schedule, and is listed in the Notices inbox
-- (schedule_binding_ambiguous) until its definition is saved again, which
-- settles it. Not guessed.
UPDATE definition_schedules
   SET schedule_uid = (SELECT CASE WHEN COUNT(*) = 1 THEN MAX(s.uid) END FROM schedules s
                        WHERE s.name = definition_schedules.source_ref)
 WHERE schedule_uid IS NULL AND source_ref IS NOT NULL AND source_ref != ''
   AND owner_source = 'cronomicon';

-- Propagation, detach, purge and every "used by" key on this column from now on.
CREATE INDEX idx_def_schedules_schedule_uid ON definition_schedules(schedule_uid)
    WHERE schedule_uid IS NOT NULL;
