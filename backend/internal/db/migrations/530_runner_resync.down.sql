-- Reverse 530. Drop the runners.resync_requested flag. SQLite ALTER TABLE
-- DROP COLUMN is supported by mattn/go-sqlite3 (precedented in 330/510/511/
-- 512/520 down). Transient operator signal; nothing else references it.
ALTER TABLE runners DROP COLUMN resync_requested;
