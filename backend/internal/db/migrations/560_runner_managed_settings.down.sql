-- Reverse 560. Drop the server-managed settings columns (SQLite ALTER TABLE
-- DROP COLUMN, precedented in 330/510/511/512/520/530/540/550 down). Operator
-- state; nothing else references it, and register/redeclare do not depend on it.
ALTER TABLE runners DROP COLUMN settings_acked_version;
ALTER TABLE runners DROP COLUMN settings_version;
ALTER TABLE runners DROP COLUMN managed_settings;
