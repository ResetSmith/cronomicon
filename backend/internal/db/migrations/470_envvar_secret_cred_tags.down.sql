-- Reverse 470. Drop the three user-authored tags columns. SQLite ALTER TABLE
-- DROP COLUMN is supported by mattn/go-sqlite3 (precedented in 280.down /
-- 290.down); tags is unindexed and unreferenced on every table, so the drop is
-- in-place and leaves the UNIQUE(key,scope) / ux_ssh_credentials_label
-- constraints and the auth_credential_id FKs intact.
ALTER TABLE env_vars DROP COLUMN tags;
ALTER TABLE secrets DROP COLUMN tags;
ALTER TABLE ssh_credentials DROP COLUMN tags;
