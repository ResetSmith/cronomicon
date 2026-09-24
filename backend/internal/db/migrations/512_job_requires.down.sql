-- Reverse 512. Drop the requirement-token columns. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 330/510/511 down).
-- Rollback discards any requires_json declarations/snapshots; the next sync
-- after re-applying 512 recomputes jobs.requires_json from git.
ALTER TABLE runs DROP COLUMN requires_json;
ALTER TABLE jobs DROP COLUMN requires_json;
