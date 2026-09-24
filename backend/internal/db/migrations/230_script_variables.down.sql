-- Reverse 230. Drop the referenced-variables column. SQLite ALTER TABLE DROP
-- COLUMN is supported by mattn/go-sqlite3 (precedented in 140.down / 210.down).
ALTER TABLE scripts DROP COLUMN variables;
