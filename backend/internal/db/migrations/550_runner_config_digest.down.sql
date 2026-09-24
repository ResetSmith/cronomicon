-- Reverse 550. Drop the runners.config_digest column (SQLite DROP COLUMN
-- table rebuild, precedented in 330/510/511/512/520/530/540 down). Derived
-- data; register/redeclare recompute it.
ALTER TABLE runners DROP COLUMN config_digest;
