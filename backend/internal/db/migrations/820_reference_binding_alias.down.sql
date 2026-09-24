-- 820 down — drop the binding alias and restore migration 590's primary key.
--
-- Reverses 820 with the same rebuild. This is a NARROWING in two ways, and both
-- are deliberate rather than lossy-by-accident:
--
--   - the alias column is dropped, so every binding reverts to injecting under
--     its own row name (the pre-Phase-A contract). A job body that had come to
--     depend on an aliased destination stops receiving it.
--   - the PK narrows back to (owner_kind, owner_source, owner_name, ref_kind,
--     ref_name), so an owner that declared the SAME row under two aliases now has
--     two rows colliding on one key. The INSERT below de-duplicates with
--     SELECT DISTINCT rather than failing the migration: after the alias is gone
--     the two rows are genuinely the same binding, so collapsing them is the only
--     meaning the data can carry.
--
-- Roll back before aliases are relied on, consistent with the repo's other
-- constraint-narrowing down migrations.
--
-- Same trigger caveat as the up: 590's cascade triggers name this table in their
-- bodies and are re-parsed during the rename, so they are dropped and recreated
-- around the rebuild.
DROP TRIGGER IF EXISTS reference_bindings_job_cleanup;
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;

CREATE TABLE reference_bindings_new (
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','script')),
    owner_source TEXT NOT NULL DEFAULT '',
    owner_name   TEXT NOT NULL,
    ref_kind     TEXT NOT NULL CHECK (ref_kind IN ('secret','var','key')),
    ref_name     TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (owner_kind, owner_source, owner_name, ref_kind, ref_name)
);
INSERT INTO reference_bindings_new
    (owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at)
    SELECT owner_kind, owner_source, owner_name, ref_kind, ref_name,
           MIN(created_by), MIN(created_at)
    FROM reference_bindings
    GROUP BY owner_kind, owner_source, owner_name, ref_kind, ref_name;
DROP TABLE reference_bindings;
ALTER TABLE reference_bindings_new RENAME TO reference_bindings;

CREATE INDEX idx_reference_bindings_owner
    ON reference_bindings (owner_kind, owner_source, owner_name);

CREATE TRIGGER reference_bindings_job_cleanup
AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

CREATE TRIGGER reference_bindings_script_cleanup
AFTER DELETE ON scripts
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'script' AND owner_name = OLD.name;
END;
