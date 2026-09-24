-- Reverse 520. Drop the runners.toolchains detail column. SQLite ALTER TABLE
-- DROP COLUMN is supported by mattn/go-sqlite3 (precedented in 330/510/511/512
-- down). Display-only data; nothing else references it.
ALTER TABLE runners DROP COLUMN toolchains;
