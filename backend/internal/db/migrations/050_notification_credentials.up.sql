-- 050 C.1: extend notification_config so the dispatcher can actually send.
-- The 004 table held only smtp_host/port/from + apprise_targets — not enough to
-- authenticate SMTP or pick a transport. Add the credential + transport columns.
-- The SMTP password is stored as an encrypted token (secrets.EncryptString,
-- envelope-encrypted with the KEK), never plaintext (C.1 secret handling).

ALTER TABLE notification_config ADD COLUMN provider          TEXT NOT NULL DEFAULT 'smtp';
ALTER TABLE notification_config ADD COLUMN smtp_encryption   TEXT NOT NULL DEFAULT 'starttls'; -- none|starttls|tls
ALTER TABLE notification_config ADD COLUMN smtp_username     TEXT;
ALTER TABLE notification_config ADD COLUMN smtp_password_enc TEXT;            -- v1: envelope token
ALTER TABLE notification_config ADD COLUMN smtp_from_name    TEXT;
ALTER TABLE notification_config ADD COLUMN smtp_recipients   TEXT NOT NULL DEFAULT '[]'; -- JSON array
ALTER TABLE notification_config ADD COLUMN apprise_enabled   INTEGER NOT NULL DEFAULT 0 CHECK (apprise_enabled IN (0,1));
ALTER TABLE notification_config ADD COLUMN apprise_url       TEXT;           -- per-config gateway override
