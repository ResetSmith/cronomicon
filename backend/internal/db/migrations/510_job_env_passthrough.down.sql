-- Reverse 510. Drop the job env-passthrough column. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 330.down and
-- earlier). Rollback discards any env_passthrough lists authored while 510
-- was live; the next sync after re-applying 510 recomputes them from git.
ALTER TABLE jobs DROP COLUMN env_passthrough;
