-- 830 Per-agency same-name rows (the runas-update plan, RA-15/RA-16/RA-19
-- — Phase E's schema half).
--
-- WHAT THIS BUYS. Today `UNIQUE (key, scope)` means one department's BECOME_PASSWORD
-- in `prod` is the ONLY BECOME_PASSWORD in `prod`. Two departments sharing a scope
-- cannot each hold their own; the second create collides. Phase A worked around that
-- with aliasing (TEAMA_SUDO → CRONOMICON_SECRET_BECOME_PASSWORD), which is the right
-- answer when rows genuinely cannot share a name. This is the other half: let them
-- share the name outright, and let a run resolve its OWN department's row.
--
--     key=BECOME_PASSWORD scope=prod owner=TeamA   ─┐ two rows, one name,
--     key=BECOME_PASSWORD scope=prod owner=TeamB   ─┘ one scope, no alias needed
--
-- WHY A SINGLE owner COLUMN AND NOT MEMBERSHIP-DRIVEN UNIQUENESS. The obvious
-- alternative — "duplicates allowed iff their membership sets are disjoint" — was
-- rejected in §2.6. Membership is a many-to-many join, so the DB cannot express the
-- invariant at all; every membership EDIT becomes a potential uniqueness violation
-- (removing a row's last agency suddenly collides it with a global sibling); and the
-- A13 cross-kind checks would have to join through membership on every create.
-- Ownership is single-valued, so uniqueness stays a plain DB fact. Membership keeps
-- its existing job — the VISIBILITY set — and the owner is simply enrolled in it,
-- because an owned row invisible to its owner is a broken state.
--
-- ⚠️ THIS IS A PURE WIDENING, AND THAT IS DELIBERATE IN A SUBTLE WAY. The new key is
-- (key, scope, owner_agency) with owner_agency NOT NULL DEFAULT '' — NOT a NULL-able
-- column, and NOT a COALESCE()-based unique INDEX. Both alternatives were considered
-- and rejected:
--
--   * A NULLABLE owner would silently REGRESS the constraint. SQLite treats NULLs as
--     distinct in a UNIQUE, so (X,'prod',NULL) could be inserted twice — two unowned
--     rows in one scope, which is precisely the ambiguity this phase exists to make
--     well-defined. NOT NULL DEFAULT '' keeps the unowned case strict.
--   * A NULL-safe index over COALESCE(scope,'') would be a NARROWING. Global scope is
--     stored as SQL NULL, so today two GLOBAL rows may share a key (SQLite's UNIQUE
--     ignores NULL pairs) — a real pre-existing hole. Closing it here would make this
--     migration able to FAIL on live data, and would entangle an unrelated fix with a
--     schema change that must not be able to reject anything. The hole is left exactly
--     as it is, to be closed on its own terms; RA-17's resolver fails closed on the
--     ambiguity regardless, which is the control that actually matters at run time.
--
-- So every existing row copies across with owner_agency='' and every pair that was
-- unique before is still unique. Nothing can be rejected.
--
-- owner_agency stores the agency **ID**, not its name: agencies can be RENAMED
-- (handleUpdateAgency), and ownership must survive that. There is deliberately no FK
-- — '' is not a valid agencies.id, so a real constraint would reject every unowned
-- row. The agency-delete path clears ownership instead.
--
-- 🔴 FK CASCADE — migration 490's note DOES NOT APPLY HERE, and believing it would
-- have destroyed data. 490 says "golang-migrate wraps each migration in a transaction
-- where PRAGMA foreign_keys is a no-op"; that was written about jobs/runs/scripts,
-- which have NO inbound foreign keys, so the claim was never actually exercised. This
-- pool opens with `_foreign_keys=on` (db.go), enforcement IS live during migrations,
-- and `secrets` / `env_vars` each carry inbound references:
--
--     secret_agencies.secret_id      ON DELETE CASCADE   → membership silently erased
--     env_var_agencies.env_var_id    ON DELETE CASCADE   → membership silently erased
--     gitlab_config.pat_secret_id    ON DELETE SET NULL  → the GitLab PAT unlinked,
--                                                          breaking repo sync
--
-- DROP TABLE therefore fires those actions before the rename can restore the name.
-- The row ids are preserved by the copy, so the references would be valid again — but
-- by then the rows are already gone. Each is stashed in a staging table and restored
-- after the rename. (Caught by the 830 round-trip test, which is the only reason this
-- is not a silent loss of every department's membership on upgrade.)
--
-- Neither table carries a trigger (checked) and neither carries a non-autoindex index,
-- so unlike migration 820 there is nothing to drop and recreate around the rename.
CREATE TABLE _830_secret_agencies AS SELECT * FROM secret_agencies;
CREATE TABLE _830_env_var_agencies AS SELECT * FROM env_var_agencies;
CREATE TABLE _830_gitlab_pat AS SELECT id, pat_secret_id FROM gitlab_config;
--
-- ⚠️ The secrets copy moves KEK-SEALED BLOBS. ciphertext / nonce / wrapped_dek /
-- kek_version are copied column-for-column with no transformation; a seal → migrate →
-- unseal round trip is regression-fenced in the migration test. A rebuild that
-- re-encoded a BLOB would silently destroy every stored secret in the installation.

-- ── secrets → add owner_agency, widen the uniqueness key ────────────────────
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
    -- '' = unowned: shared infrastructure, exactly what every pre-830 row is.
    owner_agency     TEXT NOT NULL DEFAULT '',
    UNIQUE (key, scope, owner_agency)
);
INSERT INTO secrets_new
    (id, key, scope, source, vault_ref, ciphertext, nonce, wrapped_dek, kek_version,
     created_by, created_at, last_modified_by, last_modified_at, description, tags, owner_agency)
    SELECT id, key, scope, source, vault_ref, ciphertext, nonce, wrapped_dek, kek_version,
           created_by, created_at, last_modified_by, last_modified_at, description, tags, ''
    FROM secrets;
DROP TABLE secrets;
ALTER TABLE secrets_new RENAME TO secrets;

-- ── env_vars → same treatment ───────────────────────────────────────────────
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
    owner_agency     TEXT NOT NULL DEFAULT '',
    UNIQUE (key, scope, owner_agency)
);
INSERT INTO env_vars_new
    (id, key, scope, value, created_by, created_at, last_modified_by, last_modified_at,
     description, tags, owner_agency)
    SELECT id, key, scope, value, created_by, created_at, last_modified_by, last_modified_at,
           description, tags, ''
    FROM env_vars;
DROP TABLE env_vars;
ALTER TABLE env_vars_new RENAME TO env_vars;

-- ── restore what the cascades took ─────────────────────────────────────────
-- Ids were preserved by both copies, so every stashed reference resolves again.
INSERT OR IGNORE INTO secret_agencies  SELECT * FROM _830_secret_agencies;
INSERT OR IGNORE INTO env_var_agencies SELECT * FROM _830_env_var_agencies;
UPDATE gitlab_config SET pat_secret_id = (
    SELECT b.pat_secret_id FROM _830_gitlab_pat b WHERE b.id = gitlab_config.id
) WHERE EXISTS (SELECT 1 FROM _830_gitlab_pat b WHERE b.id = gitlab_config.id);
DROP TABLE _830_secret_agencies;
DROP TABLE _830_env_var_agencies;
DROP TABLE _830_gitlab_pat;

-- ── ssh_credentials → RA-19 ────────────────────────────────────────────────
-- Labels have no scope dimension, so per-owner uniqueness here is (label, owner).
-- A department may therefore shadow a GLOBAL label, and RA-17's owned-beats-shared
-- tier applies to key resolution exactly as it does to secrets and variables.
--
-- No rebuild: the constraint is a standalone UNIQUE INDEX (migration 460), not a
-- table-level clause, so it can simply be dropped and recreated. (The plan expected
-- there to be no DB constraint here at all — there is one, and this is why the
-- app-level label check is not the only thing that had to change.)
ALTER TABLE ssh_credentials ADD COLUMN owner_agency TEXT NOT NULL DEFAULT '';
DROP INDEX ux_ssh_credentials_label;
CREATE UNIQUE INDEX ux_ssh_credentials_label ON ssh_credentials(label, owner_agency);
