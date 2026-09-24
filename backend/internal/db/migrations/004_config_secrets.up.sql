-- 004 operator-managed config + secrets (A1.1/A1.2/A1.3 DB-resident; S1/S14 secrets).
-- These are the documented exceptions to "GitLab is the source of truth" (architecture §2.2).

-- Non-secret environment variables (A1.1).
CREATE TABLE env_vars (
    id               TEXT PRIMARY KEY,
    key              TEXT NOT NULL,
    scope            TEXT,
    value            TEXT NOT NULL,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT,
    UNIQUE (key, scope)
);

-- Secrets (S1/S14). source='vault' stores only a reference; source='stored' holds
-- AES-256-GCM envelope-encrypted bytes (random DEK per secret, wrapped by the KEK).
CREATE TABLE secrets (
    id               TEXT PRIMARY KEY,
    key              TEXT NOT NULL,
    scope            TEXT,
    source           TEXT NOT NULL CHECK (source IN ('vault','stored')),
    vault_ref        TEXT,                           -- when source='vault'
    ciphertext       BLOB,                           -- when source='stored' (S14)
    nonce            BLOB,                           -- GCM nonce
    wrapped_dek      BLOB,                           -- DEK encrypted by the KEK (envelope encryption)
    kek_version      INTEGER,                        -- supports KEK rotation
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT,
    UNIQUE (key, scope)
);

-- GitLab connection config (operator-managed; contains a PAT stored as a secret ref).
CREATE TABLE gitlab_config (
    id               INTEGER PRIMARY KEY CHECK (id = 1), -- singleton row
    base_url         TEXT,
    project_path     TEXT,
    pat_secret_id    TEXT REFERENCES secrets(id) ON DELETE SET NULL,
    webhook_secret   TEXT,                              -- rotated via S15 flow
    branch           TEXT NOT NULL DEFAULT 'main',
    last_modified_by TEXT,
    last_modified_at TEXT
);

-- Notification + alert config (operator-managed).
CREATE TABLE notification_config (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    smtp_host        TEXT,
    smtp_port        INTEGER,
    smtp_from        TEXT,
    apprise_targets  TEXT NOT NULL DEFAULT '[]',        -- JSON array
    last_modified_by TEXT,
    last_modified_at TEXT
);

CREATE TABLE alert_destinations (
    id               TEXT PRIMARY KEY,
    type             TEXT NOT NULL,                     -- e.g. 'slack','email','webhook'
    target           TEXT NOT NULL,
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);

CREATE TABLE alert_config (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    condition        TEXT NOT NULL,                     -- e.g. 'job_failed','runner_offline'
    destination_id   TEXT REFERENCES alert_destinations(id) ON DELETE SET NULL,
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);

-- SSH inventory (A1.3) + bastions.
CREATE TABLE ssh_hosts (
    id               TEXT PRIMARY KEY,
    hostname         TEXT NOT NULL,
    port             INTEGER NOT NULL DEFAULT 22,
    username         TEXT,
    bastion_id       TEXT,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);

CREATE TABLE bastions (
    id               TEXT PRIMARY KEY,
    hostname         TEXT NOT NULL,
    port             INTEGER NOT NULL DEFAULT 22,
    username         TEXT,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);

-- Generic key/value for global settings (appName, timezone, maxConcurrent,
-- jobTimeout, sessionPolicy, sensitiveLogging per S7, observability toggles).
CREATE TABLE settings (
    key              TEXT PRIMARY KEY,
    value            TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);
