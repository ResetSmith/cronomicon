-- 1630_agency_vaults (2.5.1 SG3, SG-20 to SG-28).
--
-- An agency's own Vault connection. An agency without a row uses the
-- installation's (vault_config, which is also Global's). A Vault-sourced
-- secret or SSH key resolves in its OWNER agency's Vault, and an agency whose
-- row cannot be read or used is never given the installation's instead.
--
-- AppRole only (question 129): role_id_enc and secret_id_enc are sealed
-- settings tokens bound to this row (EncryptedSettingsColumns, keyed by
-- agency_id), so key rotation and log masking cover them. ca_pem is the CA
-- bundle an agency's Vault is reached with (plain: it is public). The address
-- must be on the installation's allowed Vault hosts (settings vaultAllowedHosts).
--
-- Deleted with its agency (an agency is deleted only when empty). Global has
-- none: its Vault is the installation's.
CREATE TABLE agency_vaults (
    agency_id        TEXT PRIMARY KEY REFERENCES agencies(id) ON DELETE CASCADE CHECK (agency_id <> 'global'),
    addr             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT '',
    role_id_enc      TEXT NOT NULL,
    secret_id_enc    TEXT NOT NULL,
    ca_pem           TEXT NOT NULL DEFAULT '',
    created_by       TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT NOT NULL,
    last_modified_at TEXT NOT NULL
);
