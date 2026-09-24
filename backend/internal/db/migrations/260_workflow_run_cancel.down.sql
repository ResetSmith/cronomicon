-- Reverse 260. Drop the soft-cancel columns. SQLite ALTER TABLE DROP COLUMN is
-- supported by mattn/go-sqlite3 (precedented in 220/221/240/250 .down).
ALTER TABLE workflow_runs DROP COLUMN cancelled_at;
ALTER TABLE workflow_runs DROP COLUMN cancelled;
