-- Reverse 651. Drop the script-declared run-inputs column. SQLite ALTER TABLE DROP
-- COLUMN is supported by the driver (precedented in 140.down / 210.down / 230.down).
ALTER TABLE scripts DROP COLUMN prompts_json;
