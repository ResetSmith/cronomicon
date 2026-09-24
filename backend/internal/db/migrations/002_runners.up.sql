-- 002 runner subsystem (architecture §6): registrations + registration tokens.

CREATE TABLE runners (
    id                TEXT PRIMARY KEY,             -- UUIDv7
    name              TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('online','offline','draining')),
    capabilities      TEXT NOT NULL DEFAULT '[]',   -- JSON array of run types this runner can execute (A6.3)
    load              INTEGER NOT NULL DEFAULT 0,   -- active jobs
    last_seen_at      TEXT,                         -- updated each long-poll (A6.2 poll doubles as heartbeat)
    drain_deadline_at TEXT,                         -- set when draining; force-kill after (A6.4)
    registered_at     TEXT NOT NULL,
    created_at        TEXT NOT NULL
);
CREATE INDEX idx_runners_status ON runners(status);

-- Shared registration token (A6.1), 24h expiry / rotation (§6.6). Stored as a
-- hash; the plaintext is shown once at generation and never persisted.
CREATE TABLE runner_tokens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL,                        -- sha-256 hex of the bearer token (T9)
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE INDEX idx_runner_tokens_active ON runner_tokens(revoked_at, expires_at);
