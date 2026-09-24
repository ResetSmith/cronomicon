-- 460_ssh_credentials — first-class system SSH key credentials (ssh-keys-update.md
-- SK.1). A typed entity for the private keys Cronomicon uses for automated SSH, so a
-- key stops being an opaque secret referenced by name: material is envelope-
-- encrypted with the SAME scheme as the secrets table (ciphertext/nonce/wrapped_dek/
-- kek_version → KEK rotation applies uniformly), validated on save (key_type,
-- fingerprint, public_key are DERIVED, never user-supplied), and referenced by
-- ssh_hosts/bastions via FK id instead of the legacy auth_key_env_var name string.
--
-- `source` mirrors the secrets stored|vault model: 'stored' keeps the envelope
-- columns; 'vault' keeps vault_ref (resolve path is Phase 2). v1 stores unencrypted
-- keys only — no passphrase/certificate columns are reserved (SK-D8); those become
-- additive migrations if/when needed (no speculative columns).
--
-- ssh_credentials is created BEFORE the ALTERs so the FK target exists; the new
-- auth_credential_id columns are added NULL with no default, which is the only
-- form ADD COLUMN ... REFERENCES is legal in under _foreign_keys=on. They coexist
-- PERMANENTLY with auth_key_env_var (SK-D2 as reframed by SK.17): the name path is
-- not deprecated — it is how inventory/git-imported and runner-resolved hosts
-- authenticate, so the column stays. auth_credential_id is the mechanism for
-- app-managed hosts the in-app executor dials.
CREATE TABLE ssh_credentials (
    id                TEXT PRIMARY KEY,
    label             TEXT NOT NULL,
    description       TEXT,
    source            TEXT NOT NULL CHECK (source IN ('stored','vault')),
    -- stored (envelope-encrypted private key) — same column layout as secrets:
    ciphertext        BLOB,
    nonce             BLOB,
    wrapped_dek       BLOB,
    kek_version       INTEGER,
    -- vault:
    vault_ref         TEXT,
    -- derived on save (the validation payoff; never user-supplied):
    key_type          TEXT,          -- ssh-ed25519 / ssh-rsa / ecdsa-sha2-*
    fingerprint       TEXT,          -- SHA256:...
    public_key        TEXT,          -- authorized_keys line
    created_by        TEXT,
    created_at        TEXT,
    last_modified_by  TEXT,
    last_modified_at  TEXT
);
CREATE UNIQUE INDEX ux_ssh_credentials_label ON ssh_credentials(label);
ALTER TABLE ssh_hosts ADD COLUMN auth_credential_id TEXT REFERENCES ssh_credentials(id);
ALTER TABLE bastions  ADD COLUMN auth_credential_id TEXT REFERENCES ssh_credentials(id);
