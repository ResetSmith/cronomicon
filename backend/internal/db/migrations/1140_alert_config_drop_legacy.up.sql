-- 1140_alert_config_drop_legacy — drop the six alert_config columns nothing
-- reads (the dead-code-cleanup plan, DD-9, DD-Q2 widened).
--
-- job_tags, trigger_count, trigger_window and notify_triggerer were added by
-- 040 for the prototype's full AlertRule shape. The dispatcher's matchingRules
-- (internal/notify) has never selected any of them: `tag` targeting was never
-- threaded into RunEvent, `n-failures-window` was never implemented, and
-- notify_triggerer was never read — v0.52.23 (VF-15) narrowed the API enums
-- and marked the four fields `deprecated` in the spec, keeping the columns
-- only "for stored legacy rules". name and condition are older still (004):
-- NOT NULL with no default, so every INSERT had to supply them, and the only
-- values ever written were derived from other columns (jobName+trigger, and
-- trigger again). No SELECT anywhere reads them.
--
-- Six ALTER … DROP COLUMN statements (SQLite 3.35+; the bundled library is
-- 3.53). None of the six is indexed, a PK, a FK target or in a CHECK, which
-- is what DROP COLUMN requires. destination_id (004) stays: it is a FK and
-- its removal is a different question (the alert_destinations table it points
-- at was retired by 740's per-transport last-send).
ALTER TABLE alert_config DROP COLUMN job_tags;
ALTER TABLE alert_config DROP COLUMN trigger_count;
ALTER TABLE alert_config DROP COLUMN trigger_window;
ALTER TABLE alert_config DROP COLUMN notify_triggerer;
ALTER TABLE alert_config DROP COLUMN name;
ALTER TABLE alert_config DROP COLUMN condition;
