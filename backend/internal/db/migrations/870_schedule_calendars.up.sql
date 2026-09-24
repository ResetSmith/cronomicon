-- 870 Calendar bindings + suppression provenance
-- (the calendar-update plan, CAL-2 — Phase 1A).
--
-- ── the bindings (§2.7) ────────────────────────────────────────────────────
-- Two JSON arrays of calendar names per schedule ENTRY, in two polarities:
--
--     skip_calendars ["federal-holidays"]   a fire landing on any listed day is
--                                           suppressed
--     only_calendars ["fiscal-close"]       a fire is suppressed UNLESS it lands
--                                           on a day in the union of these
--
-- Skip is a veto and is evaluated last, so setting both is legal and unambiguous
-- ("only on fiscal-close days, but never on a holiday").
--
-- WHY JSON COLUMNS AND NOT A JOIN TABLE. A join table would be keyed on
-- (owner_source, owner_kind, owner_name, entry_name, polarity, calendar_name) —
-- six columns, maintained across the delete-by-owner-then-insert compose path and
-- copied by hand through every ref expansion. The JSON column follows the existing
-- `env` precedent exactly: it copies down through ref expansion as an opaque
-- string and folds into the content hash without ceremony. The price is no
-- referential integrity to `calendars`; that is paid for at both authoring
-- boundaries (CAL-5 API 422, CAL-9 sync ValidationError) and by the CAL-16 check,
-- with §2.2's set arithmetic as the backstop.
--
-- The columns land on BOTH tables for the same reason cron/env/start_at do
-- (migrations 770, 780): `schedules` is the authored first-class catalog,
-- `definition_schedules` is the runtime table the scheduler reads, and a ref
-- expansion copies the binding down so the runtime row stays self-contained.
--
-- NULL everywhere on upgrade ⇒ no entry is bound to any calendar ⇒ strict no-op.
ALTER TABLE schedules            ADD COLUMN skip_calendars TEXT;
ALTER TABLE schedules            ADD COLUMN only_calendars TEXT;

ALTER TABLE definition_schedules ADD COLUMN skip_calendars TEXT;
ALTER TABLE definition_schedules ADD COLUMN only_calendars TEXT;

-- ── suppression provenance (CAL-29) ────────────────────────────────────────
-- The NAME of the calendar that suppressed this fire, and nothing else. NULL on
-- every run that was not calendar-suppressed, which is every row that exists
-- today.
--
-- WHY A COLUMN RATHER THAN PARSING queued_reason. The human-readable reason
-- (`suppressed by calendar "federal-holidays" (Independence Day (observed))`)
-- stays in queued_reason and stays free to be copy-edited. The History filter
-- (CAL-29, Phase 1B) and the scheduler's own per-day de-dupe (CAL-8) both query
-- THIS column instead, so "show me every run suppressed by federal-holidays this
-- fiscal year" cannot be broken by a wording change. An audit trail that a copy
-- edit can silently gut is not an audit trail.
ALTER TABLE runs           ADD COLUMN suppressed_by_calendar TEXT;
ALTER TABLE workflow_runs  ADD COLUMN suppressed_by_calendar TEXT;

-- workflow_runs has never had a queued_reason: until CAL-32 nothing could
-- suppress a scheduled WORKFLOW fire and leave a record — fireWorkflow dropped
-- even its concurrency-cap skips with a log line only, unlike the job path's
-- recordSkippedFire. A skipped workflow row with no room for its reason would be
-- an audit row that cannot say why, so the column arrives with the recorder.
ALTER TABLE workflow_runs  ADD COLUMN queued_reason TEXT;

-- The audit query is "every suppression by calendar X, newest first". Partial on
-- the provenance column so the index stays the size of the suppressions rather
-- than the size of History.
CREATE INDEX idx_runs_suppressed_by_calendar
    ON runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
CREATE INDEX idx_workflow_runs_suppressed_by_calendar
    ON workflow_runs(suppressed_by_calendar, created_at)
    WHERE suppressed_by_calendar IS NOT NULL;
