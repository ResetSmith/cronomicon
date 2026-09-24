-- 020 runner subsystem augment (B4): add columns missing from 002 for full API
-- compliance (os, version, max_concurrent, registration_token_id), add
-- runner_id to runs for dispatch tracking, and add a registration_tokens_reg
-- table for the shared bootstrap/registration token (distinct from per-runner
-- tokens which live in runner_tokens). Also adds sensitive_logging flag to
-- runs for per-run redaction override (T7).

-- Augment runners table with fields from the openapi Runner schema.
-- SQLite ALTER TABLE supports ADD COLUMN only.
ALTER TABLE runners ADD COLUMN os TEXT NOT NULL DEFAULT 'Linux' CHECK (os IN ('Linux','Windows'));
ALTER TABLE runners ADD COLUMN version TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN max_concurrent INTEGER NOT NULL DEFAULT 5;
ALTER TABLE runners ADD COLUMN registration_token_id INTEGER; -- FK to runner_tokens.id (provenance A6.1)

-- Add runner_id to runs: B4 sets this on claim (queued→running transition).
-- Text FK matching runners.id (UUIDv7 TEXT primary key).
ALTER TABLE runs ADD COLUMN runner_id TEXT REFERENCES runners(id) ON DELETE SET NULL;

-- Shared registration tokens table (distinct from per-runner bearer tokens).
-- The bootstrap flow: operator calls GET/POST /runners/registration-token;
-- runners call POST /runners/register with the token as a Bearer credential.
-- We store the hash only; plaintext shown once (A6.1).
CREATE TABLE IF NOT EXISTS registration_tokens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL,                   -- sha-256 hex of the amt_reg_* token
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_registration_tokens_active ON registration_tokens(revoked_at, expires_at);
