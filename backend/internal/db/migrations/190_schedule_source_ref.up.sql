-- 190 Schedule edit propagation (schedule-builder.md — D1c).
--
-- scheduleRefs are SNAPSHOTTED into definition_schedules at resolve time (sync's
-- mergeScheduleRefs, compose's resolveComposeSchedules), so editing a first-class
-- schedule's cron would otherwise leave every referencing job/workflow on the
-- stale cron. source_ref records which first-class schedule an entry was expanded
-- from (NULL = an inline entry) so a schedule edit can propagate PRECISELY:
--   UPDATE definition_schedules SET cron=?, env=? WHERE source_ref=?
-- without clobbering an inline entry that coincidentally shares the name. It also
-- makes usedBy/usedByCount exact (count by source_ref, not the name-match
-- approximation Phase 1 used).
--
-- Additive (ALTER ADD COLUMN) — no rebuild. NULL ⇒ inline entry (not ref-expanded).
ALTER TABLE definition_schedules ADD COLUMN source_ref TEXT;

-- Backfill existing references: an entry whose name matches a first-class schedule
-- in the catalog is (approximately) a ref expansion — link it. Inline entries whose
-- name does not exist in `schedules` stay NULL. This mirrors the Phase-1 name-match
-- approximation for pre-existing rows; new rows carry an exact source_ref.
UPDATE definition_schedules SET source_ref = name WHERE name IN (SELECT name FROM schedules);
