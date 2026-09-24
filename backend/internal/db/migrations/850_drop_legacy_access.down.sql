-- Reverse 850 — SHAPE ONLY. This cannot restore access.
--
-- The tables come back empty, exactly as migration 003 created them. Nothing
-- refills them, and nothing in the running code writes them any more: the
-- endpoints, the seed writes, and `auth.rolesForGroups` were all deleted in
-- v0.57.8. An instance rolled back to here has two empty legacy tables and its
-- `access_grants` rows still intact and still authoritative.
--
-- That is deliberate, and it is the only honest option. Re-deriving the two-axis
-- form from grants is not a migration problem that more SQL would solve — the
-- target form is strictly less expressive. `access_grants` pairs a role with the
-- agency it applies to, one row at a time; `ad_group_mappings` × `scope_restrictions`
-- union the two axes independently. Projecting grants back onto them hands every
-- role the union of every scope any grant gave it, which re-opens the exact
-- cross-product leak the model was built to close. A down migration that silently
-- WIDENED access would be far worse than one that restores an empty shell.
--
-- If you genuinely need the old model back, restore from a backup taken before
-- v0.57.8. That is the supported path.

CREATE TABLE IF NOT EXISTS scope_restrictions (
    role    TEXT NOT NULL,
    scope   TEXT NOT NULL,
    allowed INTEGER NOT NULL DEFAULT 0 CHECK (allowed IN (0,1)),
    PRIMARY KEY (role, scope)
);

CREATE TABLE IF NOT EXISTS ad_group_mappings (
    id               TEXT PRIMARY KEY,
    ad_group         TEXT NOT NULL,
    role             TEXT NOT NULL,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_ad_group_mappings_group ON ad_group_mappings(ad_group);
