-- 020 down: SQLite does not support DROP COLUMN before 3.35.0, and golang-migrate
-- sqlite3 driver wraps each migration in a transaction where most DDL is allowed.
-- We recreate the affected tables without the added columns.

-- Drop registration_tokens table added in this migration.
DROP TABLE IF EXISTS registration_tokens;

-- Note: SQLite does not support dropping columns. The runner columns (os, version,
-- max_concurrent, registration_token_id) and runs.runner_id added in this
-- migration cannot be removed by DOWN without full table recreation, which risks
-- data loss. In practice DOWN migrations are only used in dev; production rollback
-- would restore a backup. Therefore we leave those columns in place on DOWN.
