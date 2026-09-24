-- 003 scopes + access control (A3.2 honest view, A5 role-level scope access,
-- S8 local scopes, S9 rename cascade target).

-- Scopes (inventories). source='git' are read from GitLab; source='amadeus' are
-- operator-managed in the UI and live here (S8).
CREATE TABLE scopes (
    id               TEXT PRIMARY KEY,              -- UUIDv7
    name             TEXT NOT NULL UNIQUE,
    source           TEXT NOT NULL CHECK (source IN ('git','amadeus')),
    description      TEXT,
    supported_types  TEXT NOT NULL DEFAULT '["bash"]', -- JSON array; bash floor (S10)
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,                           -- S4
    last_modified_at TEXT                            -- S4
);

-- Hosts for amadeus-source scopes (git scopes resolve hosts from the inventory file).
CREATE TABLE scope_hosts (
    scope_id TEXT NOT NULL REFERENCES scopes(id) ON DELETE CASCADE,
    host     TEXT NOT NULL,
    PRIMARY KEY (scope_id, host)
);

-- Role → scope access matrix (A5 role-level only; V2-5 adds user-level overrides).
CREATE TABLE scope_restrictions (
    role    TEXT NOT NULL,
    scope   TEXT NOT NULL,
    allowed INTEGER NOT NULL DEFAULT 0 CHECK (allowed IN (0,1)),
    PRIMARY KEY (role, scope)
);

-- AD group → role mappings (operator-managed; architecture §2.2).
CREATE TABLE ad_group_mappings (
    id               TEXT PRIMARY KEY,
    ad_group         TEXT NOT NULL,
    role             TEXT NOT NULL,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);
CREATE INDEX idx_ad_group_mappings_group ON ad_group_mappings(ad_group);

-- Honest View / Recent Logins (A3.2): a user appears only after first OIDC login.
CREATE TABLE recent_logins (
    email        TEXT PRIMARY KEY,
    display_name TEXT,
    groups       TEXT NOT NULL DEFAULT '[]',         -- JSON array from the OIDC groups claim
    first_seen_at TEXT NOT NULL,
    last_login_at TEXT NOT NULL
);
