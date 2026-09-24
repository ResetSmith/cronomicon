-- Reverse 280. Drop the user-authored tags column. SQLite ALTER TABLE DROP COLUMN
-- is supported by mattn/go-sqlite3 (precedented in 140.down / 210.down / 230.down).
ALTER TABLE scripts DROP COLUMN tags;
