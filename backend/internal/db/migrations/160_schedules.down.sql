-- Reverse 160. mattn/go-sqlite3 supports ALTER TABLE DROP COLUMN (070 precedent).
ALTER TABLE git_sync_events DROP COLUMN schedules_synced;
DROP TABLE IF EXISTS schedules;
