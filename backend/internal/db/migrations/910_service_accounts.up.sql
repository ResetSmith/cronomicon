-- 910_service_accounts — machine principals for the inbound trigger API (ET-A).
--
-- WHY THIS TABLE EXISTS. Every API call up to now rides an operator SSO session (or
-- the dev bypass). A monitoring system, a ticketing tool or a CI job had no way
-- to start a run at all — the only inbound webhook in the tree is GitLab's, and
-- that one kicks a git sync rather than authenticating anybody. This table is
-- the machine half of the identity story.
--
-- A TOKEN IS A PRINCIPAL, NOT A BYPASS (PF-Q1). The columns are deliberately
-- the SHAPE of access_grants (810): a role plus either an agency or the "*"
-- sentinel, with the identical one-shape-per-row CHECK. A token therefore
-- resolves through auth.AgencyScopes to exactly the Identity a human with the
-- same grant would carry, and every downstream gate — requireCan, Can /
-- CanAnywhere / CanUnbound, the scope-where fragments, the audit actor — reads
-- it without a single change. There is no second authorization vocabulary and
-- no per-job allowlist: a token restricted to one job is a custom role plus a
-- narrow agency, which is self-documenting in a way a loose allowlist is not.
--
-- ONE ROLE, ONE WHERE. access_grants keys on ad_group and a user may hold
-- several; a service account is a single principal, so it carries exactly one
-- grant. Widening it later means a second table, not a nullable column.
--
-- THE HASH IS THE CREDENTIAL. Only the hex SHA-256 is stored (auth.HashToken,
-- the same function runner tokens use); the plaintext is shown once at creation
-- and is unrecoverable afterwards, exactly as registration tokens work (540).
-- token_hash is UNIQUE so a collision surfaces as a write error rather than as
-- two principals sharing one credential.
--
-- Additive: a new table only, no rebuild of anything.
CREATE TABLE service_accounts (
    id           TEXT PRIMARY KEY,              -- UUIDv7 (db.NewID)
    name         TEXT NOT NULL UNIQUE,          -- operator-facing label; also the audit actor (svc:<name>)
    description  TEXT,
    token_hash   TEXT NOT NULL UNIQUE,          -- hex sha256; plaintext never stored
    role         TEXT NOT NULL,                 -- same vocabulary as access_grants.role
    agency_id    TEXT REFERENCES agencies(id) ON DELETE CASCADE,
    all_scopes   INTEGER NOT NULL DEFAULT 0 CHECK (all_scopes IN (0,1)),
    created_by   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    -- NULL means no expiry. Unlike a registration token (which is single-use and
    -- always 24h) a service account is standing infrastructure; forcing an expiry
    -- would mean an integration silently stops at 3am with no operator present.
    -- An expiry is offered and encouraged in the UI, not imposed here.
    expires_at   TEXT,
    revoked_at   TEXT,
    -- Stamped on every successful authentication, best-effort and never in the
    -- request's critical path. This is the column that answers "is this token
    -- still used, or can I revoke it?" — the question that otherwise keeps dead
    -- credentials alive for years.
    last_used_at TEXT,
    -- Exactly one shape per row, copied verbatim from access_grants (810): an
    -- agency, or the "*" sentinel. Keeps A5's tri-state intact and makes a row
    -- readable without a join.
    CHECK ((agency_id IS NOT NULL) + all_scopes = 1)
);

-- The authentication lookup: hash first, then liveness. Matches the shape of
-- idx_registration_tokens_active.
CREATE INDEX idx_service_accounts_active ON service_accounts(token_hash, revoked_at, expires_at);
