-- Reverse 210. Drop the body-lint warnings column. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 140.down).
ALTER TABLE scripts DROP COLUMN warnings;
