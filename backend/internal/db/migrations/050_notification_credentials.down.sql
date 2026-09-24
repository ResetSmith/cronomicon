-- Reverse 050. SQLite can't DROP COLUMN before 3.35; recreate the 004 shape.
-- (Down-migrations are for dev/test only; production recovery is restore-from-backup.)
ALTER TABLE notification_config DROP COLUMN provider;
ALTER TABLE notification_config DROP COLUMN smtp_encryption;
ALTER TABLE notification_config DROP COLUMN smtp_username;
ALTER TABLE notification_config DROP COLUMN smtp_password_enc;
ALTER TABLE notification_config DROP COLUMN smtp_from_name;
ALTER TABLE notification_config DROP COLUMN smtp_recipients;
ALTER TABLE notification_config DROP COLUMN apprise_enabled;
ALTER TABLE notification_config DROP COLUMN apprise_url;
