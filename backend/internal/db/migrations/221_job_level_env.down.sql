-- Reverse 221. Drop the job-level env column. SQLite ALTER TABLE DROP COLUMN is
-- supported by mattn/go-sqlite3 (precedented in 140.down / 180.down / 210.down /
-- 220.down). Rollback discards any job-level env authored while 221 was live.
ALTER TABLE jobs DROP COLUMN env_json;
