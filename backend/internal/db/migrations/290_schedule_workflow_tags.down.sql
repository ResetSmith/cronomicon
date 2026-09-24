-- Reverse 290. Drop the user-authored tags columns (workflows then schedules).
-- SQLite ALTER TABLE DROP COLUMN is supported by mattn/go-sqlite3 (precedented in
-- 140.down / 210.down / 230.down / 280.down) and is performed as a table rebuild —
-- TestMigrate290TagsRoundTrip exercises it with data present.
ALTER TABLE workflows DROP COLUMN tags;
ALTER TABLE schedules DROP COLUMN tags;
