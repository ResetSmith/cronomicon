-- 040 B6: add missing columns to support the full OpenAPI settings/secrets surface.
-- The base tables (env_vars, secrets, alert_config, alert_destinations, ssh_hosts,
-- bastions) were created in 004; this migration fills in the columns that the
-- OpenAPI contract requires but which the initial 004 omitted.

-- alert_config → full AlertRule shape
ALTER TABLE alert_config ADD COLUMN target_mode   TEXT NOT NULL DEFAULT 'all';
ALTER TABLE alert_config ADD COLUMN job_name      TEXT;
ALTER TABLE alert_config ADD COLUMN job_tags      TEXT NOT NULL DEFAULT '[]';   -- JSON array
ALTER TABLE alert_config ADD COLUMN trigger       TEXT NOT NULL DEFAULT 'failure';
ALTER TABLE alert_config ADD COLUMN trigger_count INTEGER;
ALTER TABLE alert_config ADD COLUMN trigger_window TEXT;
ALTER TABLE alert_config ADD COLUMN channels      TEXT NOT NULL DEFAULT '["in-app"]'; -- JSON array
ALTER TABLE alert_config ADD COLUMN recipients    TEXT;
ALTER TABLE alert_config ADD COLUMN notify_triggerer INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alert_config ADD COLUMN owner         TEXT;

-- alert_destinations → full AlertDestination shape
ALTER TABLE alert_destinations ADD COLUMN label       TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_destinations ADD COLUMN config      TEXT NOT NULL DEFAULT '{}'; -- JSON object
ALTER TABLE alert_destinations ADD COLUMN last_fired_at TEXT;
ALTER TABLE alert_destinations ADD COLUMN last_status  TEXT;

-- ssh_hosts → full SshHost shape
ALTER TABLE ssh_hosts ADD COLUMN address        TEXT;
ALTER TABLE ssh_hosts ADD COLUMN os             TEXT;
ALTER TABLE ssh_hosts ADD COLUMN via            TEXT;          -- bastion name (nullable)
ALTER TABLE ssh_hosts ADD COLUMN auth_key_env_var TEXT;

-- bastions → full SshBastion shape
ALTER TABLE bastions ADD COLUMN name          TEXT NOT NULL DEFAULT '';
ALTER TABLE bastions ADD COLUMN address       TEXT NOT NULL DEFAULT '';
ALTER TABLE bastions ADD COLUMN zone          TEXT;
ALTER TABLE bastions ADD COLUMN auth_key_env_var TEXT;
