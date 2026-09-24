-- Reverse 240. Drop the per-run step-graph snapshot columns. SQLite ALTER TABLE
-- DROP COLUMN is supported by mattn/go-sqlite3 (precedented in 140.down / 180.down
-- / 210.down / 220.down / 221.down). Rollback discards any snapshots captured while
-- 240 was live (runs fall back to the flat timeline).
ALTER TABLE workflow_runs DROP COLUMN steps_hash;
ALTER TABLE workflow_runs DROP COLUMN steps_snapshot;
