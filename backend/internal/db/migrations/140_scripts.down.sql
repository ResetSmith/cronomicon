-- Reverse 140. SQLite (mattn/go-sqlite3) supports ALTER TABLE DROP COLUMN; same
-- pattern as 070's down. Drop in reverse dependency order.
ALTER TABLE git_sync_events DROP COLUMN scripts_synced;
ALTER TABLE runs DROP COLUMN content_hash;
ALTER TABLE runs DROP COLUMN script_ref;
ALTER TABLE jobs DROP COLUMN content_hash;
ALTER TABLE jobs DROP COLUMN script_ref;
DROP TABLE IF EXISTS scripts;
