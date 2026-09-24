-- AW-1 (the schedule-update plan, Phase 1) — schedule activation windows.
--
-- start_at / end_at bound WHEN a schedule entry is allowed to fire, without
-- touching the cron expression itself: a NULL start means "active immediately"
-- (every pre-existing row, so this migration is a strict no-op on upgrade) and
-- a NULL end means "never expires". Both are RFC3339 UTC instants — unlike the
-- cron fields, which are evaluated in the effective app zone, the window bounds
-- are absolute and unaffected by a timezone change.
--
-- The columns land on BOTH tables for the same reason cron/env do: `schedules`
-- is the authored first-class catalog, `definition_schedules` is the runtime
-- expansion the scheduler actually reads, and a ref expansion copies the window
-- down so the runtime row stays self-contained (source_ref propagation, AW-5).
--
-- A past start_at is deliberately inert — there is no catch-up/backfill fire
-- (AW-Q2) — and an elapsed end_at leaves the entry registered but silent
-- (AW-Q7): it stays listed as `expired` rather than being auto-deleted, since
-- git-source rows must round-trip regardless.
ALTER TABLE schedules ADD COLUMN start_at TEXT;
ALTER TABLE schedules ADD COLUMN end_at TEXT;

ALTER TABLE definition_schedules ADD COLUMN start_at TEXT;
ALTER TABLE definition_schedules ADD COLUMN end_at TEXT;
