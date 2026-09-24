-- Reverse 511. Drop the checkout-project columns. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 330/510 down).
-- Rollback discards any project_root claims and per-run checkout snapshots;
-- the next sync after re-applying 511 recomputes project_root from git.
ALTER TABLE runs    DROP COLUMN checkout_entry;
ALTER TABLE runs    DROP COLUMN checkout_sha;
ALTER TABLE jobs    DROP COLUMN project_root;
ALTER TABLE scripts DROP COLUMN project_root;
