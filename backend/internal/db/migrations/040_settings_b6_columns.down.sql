-- 040 down: SQLite does not support DROP COLUMN on tables with CHECK constraints
-- or composite keys in all versions, but go-migrate requires a down file.
-- The safest rollback for SQLite ALTER ADD COLUMN is to recreate the tables;
-- for CI simplicity we just note this is a forward-only migration in practice.
SELECT 1; -- no-op: SQLite cannot drop columns added by ALTER TABLE ADD COLUMN
