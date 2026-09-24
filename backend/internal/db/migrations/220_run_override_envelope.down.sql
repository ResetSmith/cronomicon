-- Reverse 220. Drop the per-run override envelope column. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 140.down / 180.down / 210.down).
ALTER TABLE runs DROP COLUMN override_json;
