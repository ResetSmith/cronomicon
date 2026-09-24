-- Reverse 660. Drop the per-job enforcement column; every job reverts to the
-- warn-only UDV4 default. SQLite ALTER TABLE DROP COLUMN is supported by the driver
-- (precedented in 140.down / 210.down / 230.down / 651.down).
ALTER TABLE jobs DROP COLUMN prompt_enforcement;
