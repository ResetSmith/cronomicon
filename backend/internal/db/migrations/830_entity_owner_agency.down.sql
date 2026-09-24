-- 830 down — drop entity ownership and restore the (key, scope) uniqueness key.
--
-- This is a NARROWING: two departments' same-named rows in one scope become a
-- uniqueness violation the moment ownership is gone. The copies below therefore use
-- GROUP BY to keep ONE row per (key, scope) rather than letting the migration fail
-- half-way through an upgrade rollback.
--
-- ⚠️ THAT DISCARDS DATA, and there is no version of this that does not. Once the
-- owner column is gone the rows are genuinely indistinguishable, so something has to
-- be dropped; the alternative is a rollback that aborts and leaves a dirty schema.
-- MIN(id) makes the choice deterministic rather than arbitrary — but the honest
-- summary is: roll back BEFORE two departments start sharing a name, or export the
-- losing rows first. Every row a rollback drops is a secret or variable someone's
-- runs depend on.
--
-- The unowned catalogue — every installation that has not yet used Phase E — rolls
-- back losslessly, because '' is the only owner present and the GROUP BY collapses
-- nothing.
--
-- 🔴 Same FK-cascade hazard as the up (see 830_..._up.sql): this pool enforces foreign
-- keys during migrations, so DROP TABLE fires ON DELETE CASCADE into the membership
-- tables and ON DELETE SET NULL into gitlab_config.pat_secret_id. Stash and restore.
-- The restore is INSERT OR IGNORE + a FK-valid filter, because a rollback that
-- collapses duplicates leaves membership rows pointing at ids that no longer exist.
CREATE TABLE _830d_secret_agencies AS SELECT * FROM secret_agencies;
CREATE TABLE _830d_env_var_agencies AS SELECT * FROM env_var_agencies;
CREATE TABLE _830d_gitlab_pat AS SELECT id, pat_secret_id FROM gitlab_config;

CREATE TABLE secrets_new (
    id               TEXT PRIMARY KEY,
    key              TEXT NOT NULL,
    scope            TEXT,
    source           TEXT NOT NULL CHECK (source IN ('vault','stored')),
    vault_ref        TEXT,
    ciphertext       BLOB,
    nonce            BLOB,
    wrapped_dek      BLOB,
    kek_version      INTEGER,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT,
    description      TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    UNIQUE (key, scope)
);
INSERT INTO secrets_new
    (id, key, scope, source, vault_ref, ciphertext, nonce, wrapped_dek, kek_version,
     created_by, created_at, last_modified_by, last_modified_at, description, tags)
    SELECT id, key, scope, source, vault_ref, ciphertext, nonce, wrapped_dek, kek_version,
           created_by, created_at, last_modified_by, last_modified_at, description, tags
    FROM secrets
    WHERE id IN (SELECT MIN(id) FROM secrets GROUP BY key, scope);
DROP TABLE secrets;
ALTER TABLE secrets_new RENAME TO secrets;

CREATE TABLE env_vars_new (
    id               TEXT PRIMARY KEY,
    key              TEXT NOT NULL,
    scope            TEXT,
    value            TEXT NOT NULL,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT,
    description      TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    UNIQUE (key, scope)
);
INSERT INTO env_vars_new
    (id, key, scope, value, created_by, created_at, last_modified_by, last_modified_at,
     description, tags)
    SELECT id, key, scope, value, created_by, created_at, last_modified_by, last_modified_at,
           description, tags
    FROM env_vars
    WHERE id IN (SELECT MIN(id) FROM env_vars GROUP BY key, scope);
DROP TABLE env_vars;
ALTER TABLE env_vars_new RENAME TO env_vars;

-- Restore only what still points at a surviving row: the GROUP BY above may have
-- dropped the losing duplicate, and its membership must not be resurrected against a
-- missing id (that would fail the FK and abort the rollback).
INSERT OR IGNORE INTO secret_agencies
    SELECT * FROM _830d_secret_agencies b WHERE b.secret_id IN (SELECT id FROM secrets);
INSERT OR IGNORE INTO env_var_agencies
    SELECT * FROM _830d_env_var_agencies b WHERE b.env_var_id IN (SELECT id FROM env_vars);
UPDATE gitlab_config SET pat_secret_id = (
    SELECT b.pat_secret_id FROM _830d_gitlab_pat b
    WHERE b.id = gitlab_config.id AND b.pat_secret_id IN (SELECT id FROM secrets)
) WHERE EXISTS (SELECT 1 FROM _830d_gitlab_pat b WHERE b.id = gitlab_config.id);
DROP TABLE _830d_secret_agencies;
DROP TABLE _830d_env_var_agencies;
DROP TABLE _830d_gitlab_pat;

-- ssh_credentials: drop the owner column via rebuild-free ALTER (SQLite 3.35+
-- supports DROP COLUMN), and restore the label-only unique index. Same narrowing
-- caveat: two departments' same-labelled keys collide, so the index is recreated
-- AFTER the duplicates are removed.
DELETE FROM ssh_credentials
WHERE id NOT IN (SELECT MIN(id) FROM ssh_credentials GROUP BY label);
DROP INDEX ux_ssh_credentials_label;
ALTER TABLE ssh_credentials DROP COLUMN owner_agency;
CREATE UNIQUE INDEX ux_ssh_credentials_label ON ssh_credentials(label);
