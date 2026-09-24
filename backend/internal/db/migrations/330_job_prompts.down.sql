-- Reverse 330. Drop the job prompt-variables column. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 140.down / 210.down /
-- 221.down / 230.down). Rollback discards any prompts authored while 330 was live.
ALTER TABLE jobs DROP COLUMN prompts_json;
