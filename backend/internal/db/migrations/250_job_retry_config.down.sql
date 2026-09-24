-- Reverse 250. Drop the job-level retry-config columns. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 220/221/240 .down).
ALTER TABLE jobs DROP COLUMN continue_on_error;
ALTER TABLE jobs DROP COLUMN backoff_seconds;
