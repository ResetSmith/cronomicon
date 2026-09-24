-- 060 Settings Endpoints: gitlab_config schema updates, vault_config, log_storage_config

-- Update gitlab_config table to align with OpenAPI spec
ALTER TABLE gitlab_config ADD COLUMN pat_enc TEXT;
ALTER TABLE gitlab_config ADD COLUMN bot_name TEXT NOT NULL DEFAULT 'amadeus-bot';
ALTER TABLE gitlab_config ADD COLUMN bot_email TEXT NOT NULL DEFAULT 'amadeus-bot@amadeus.io';
ALTER TABLE gitlab_config ADD COLUMN write_branch TEXT NOT NULL DEFAULT 'main';
ALTER TABLE gitlab_config ADD COLUMN repo_url TEXT NOT NULL DEFAULT '';
ALTER TABLE gitlab_config ADD COLUMN token_expiry_notify_days INTEGER NOT NULL DEFAULT 7;
ALTER TABLE gitlab_config ADD COLUMN webhook_enabled INTEGER NOT NULL DEFAULT 0 CHECK (webhook_enabled IN (0,1));
ALTER TABLE gitlab_config ADD COLUMN webhook_events_push INTEGER NOT NULL DEFAULT 0 CHECK (webhook_events_push IN (0,1));
ALTER TABLE gitlab_config ADD COLUMN webhook_events_mr INTEGER NOT NULL DEFAULT 0 CHECK (webhook_events_mr IN (0,1));
ALTER TABLE gitlab_config ADD COLUMN webhook_events_tag INTEGER NOT NULL DEFAULT 0 CHECK (webhook_events_tag IN (0,1));
ALTER TABLE gitlab_config ADD COLUMN webhook_secret_enc TEXT;
ALTER TABLE gitlab_config ADD COLUMN webhook_secret_prev_enc TEXT;
ALTER TABLE gitlab_config ADD COLUMN webhook_overlap_until TEXT;

-- Create vault_config table
CREATE TABLE vault_config (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    addr             TEXT NOT NULL DEFAULT '',
    auth_method      TEXT NOT NULL DEFAULT 'approle' CHECK (auth_method IN ('approle', 'token')),
    role_id          TEXT NOT NULL DEFAULT '',
    secret_id_enc    TEXT,
    namespace        TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT
);

-- Create log_storage_config table
CREATE TABLE log_storage_config (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    backend          TEXT NOT NULL DEFAULT 'local' CHECK (backend IN ('local', 's3')),
    local_path       TEXT NOT NULL DEFAULT '/var/lib/amadeus/logs',
    s3_endpoint      TEXT,
    s3_bucket        TEXT,
    s3_region        TEXT,
    s3_access_key    TEXT,
    s3_secret_key_enc TEXT,
    s3_prefix        TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT
);
