-- Reverse 1140 — re-add the six columns with 040's / 004's shapes, empty.
--
-- LOSSY by nature, and it does not matter: no code path has read any of these
-- columns since v0.52.23, so the values dropped by 1140 were never consulted.
-- name and condition come back NOT NULL with a '' default (004 had no
-- default — an ADD COLUMN NOT NULL needs one), which is what a pre-1140
-- INSERT would have to fill anyway; a rolled-back binary writes the derived
-- values again on the next create/update.
ALTER TABLE alert_config ADD COLUMN name             TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_config ADD COLUMN condition        TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_config ADD COLUMN job_tags         TEXT NOT NULL DEFAULT '[]';
ALTER TABLE alert_config ADD COLUMN trigger_count    INTEGER;
ALTER TABLE alert_config ADD COLUMN trigger_window   TEXT;
ALTER TABLE alert_config ADD COLUMN notify_triggerer INTEGER NOT NULL DEFAULT 0 CHECK (notify_triggerer IN (0,1));
