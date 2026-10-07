-- 1240_agency_vault_prefixes — the Vault paths an agency may name (LR-80).
--
-- The installation has one Vault connection. Whatever its credential can read,
-- any secret or SSH key that names the path can have delivered to a run, so
-- since 2.2.2 only a global administrator could create or edit a Vault-backed
-- secret (GC-8) or key (2.2.3). That closed the hole and left an agency unable
-- to manage its own Vault-backed secrets at all.
--
-- A global administrator assigns each agency the prefixes it may use; an
-- agency's administrators then write Vault-backed secrets and keys inside them.
-- An agency with no row here can name no Vault path. Global needs none: its
-- Vault-backed rows are a global administrator's, unrestricted, as before.
--
-- The prefix is stored normalised (internal/vaultpath: no leading or trailing
-- slash, no empty, "." or ".." segment) and matched on whole segments.
--
-- Checked when a secret or key is written or moved, NOT when a run resolves it
-- (LR-81): removing a prefix does not revoke what was already written under it.
-- The notices inbox lists agency-owned rows that sit outside their agency's
-- prefixes — which, on the day of the upgrade, is every one of them.
CREATE TABLE agency_vault_prefixes (
    agency_id  TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
    prefix     TEXT NOT NULL CHECK (prefix <> ''),
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (agency_id, prefix)
);
