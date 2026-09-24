-- 860 Working calendars (the calendar-update plan, CAL-1 — Phase 1A).
--
-- A calendar is a named set of WALL-CLOCK DATES, each optionally labelled:
--
--     federal-holidays
--       2026-01-01  New Year's Day
--       2026-07-03  Independence Day (observed)
--
-- Not a time range, not a cron, not a duration. Migration 870 binds calendars to
-- schedule entries in two polarities (skip these days / only these days) and the
-- scheduler suppresses matching fires at fire() time.
--
-- WHY ENUMERATED DAYS AND NOT GENERATED RULES. Federal holidays are rule-defined
-- in principle ("third Monday in January") but the tail is ugly — weekend
-- observance shifts, Inauguration Day every fourth year and DC-only, executive
-- order closures. A generator that gets 90% right is worse than a list somebody
-- signed off on. `calendar_days.rule` exists as a nullable PROVENANCE column so a
-- future generator (§4) can populate rows without a schema change; nothing reads
-- it in Phase 1.
--
-- ⚠️ AMADEUS SHIPS NO HOLIDAY CONTENT (CAL-Q8) — no seeded row here, and no
-- importable file anywhere in the tree. Operators author their own dates and own
-- them end to end, so a wrong or stale list is never one we supplied. The direct
-- consequence is that an unrenewed calendar is this feature's likeliest failure:
-- a `skip` calendar whose last day is in the past simply stops suppressing and
-- holiday runs quietly resume. CAL-16 (Phase 1B) is the only detector.
--
-- ── source: a one-line hedge, deliberately unexposed ────────────────────────
-- Git-authored calendars are NOT PLANNED (CAL-Q2) — calendars are operator
-- authored, source='amadeus', full stop. The column and the (source, name) PK
-- survive anyway because adding them LATER would mean rebuilding a table that
-- `calendar_days` holds an ON DELETE CASCADE foreign key into, and that rebuild
-- is precisely the hazard migration 830 documents at length. One column now is
-- cheaper than a cascade-safe rebuild later. No API surface and no UI exposes
-- it; a bare calendar name is unambiguous by construction (CAL-Q7).
--
-- 🔴 IF `calendars` IS EVER REBUILT (the RA-series cascade lesson, migration 830):
-- this pool opens with `_foreign_keys=on` (db.go), so enforcement IS live during
-- migrations. DROP TABLE calendars fires ON DELETE CASCADE into calendar_days and
-- erases every day of every calendar before the rename can restore the name. Stash
-- calendar_days into a staging table and restore it after the rename, and
-- re-create the FK in the new table definition — a rebuild that forgets the FK
-- silently turns cascade deletes into orphan rows.
CREATE TABLE calendars (
    source           TEXT NOT NULL DEFAULT 'amadeus',
    name             TEXT NOT NULL,
    description      TEXT,
    -- CAL-27 (§2.8) — the global tier. A global calendar's days are unioned into
    -- EVERY entry's effective skip set, whatever that entry's own bindings say:
    -- one checkbox enforces a change freeze without editing N schedules.
    --
    -- SKIP POLARITY ONLY, and the API enforces it (CAL-27, Phase 1B): a global
    -- `only` calendar would mean nothing in the system ever runs except on listed
    -- days — a system-wide outage shaped like a feature, one checkbox away.
    global           INTEGER NOT NULL DEFAULT 0 CHECK (global IN (0, 1)),
    -- CAL-Q4 — whether `only`-mode suppressions are recorded in History.
    -- `skip`-mode suppressions ALWAYS record (that is the compliance question:
    -- "prove we did not patch on the holiday, deliberately"). `only`-mode
    -- suppressions are the normal case rather than the exception — a
    -- weekdays-only entry suppresses ten times every weekend — so recording them
    -- is opt-in per calendar, or the signal drowns in its own noise.
    record_suppressed INTEGER NOT NULL DEFAULT 0 CHECK (record_suppressed IN (0, 1)),
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT,
    PRIMARY KEY (source, name)
);

-- One row per day per calendar. `label` is what History and the Upcoming tab
-- show as the reason ("Independence Day (observed)"); `rule` is unused
-- provenance for a future generator.
--
-- day is strict 'YYYY-MM-DD' wall-clock text, matched against the fire instant
-- rendered in the EFFECTIVE APP TIMEZONE — never UTC. A holiday is a wall-clock
-- day; matching in UTC would suppress the wrong side of midnight for every
-- deployment east or west of Greenwich.
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

-- The resolver loads by calendar name; this index serves the inverse question
-- ("which calendars cover today?"), which is what the CAL-28 active-global
-- banner and the CAL-16 coverage check ask.
CREATE INDEX idx_calendar_days_day ON calendar_days(day);
