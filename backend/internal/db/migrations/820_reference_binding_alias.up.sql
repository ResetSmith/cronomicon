-- 820 Reference binding ALIAS (the runas-update plan, RA-2 — the
-- storage half of Phase A aliasing).
--
-- A binding has always injected under its own row name: bind the secret `X` and
-- the run gets AMADEUS_SECRET_X, full stop. That makes one shared job body
-- consumable by exactly ONE department's credential — a playbook that hardcodes
-- lookup('env','AMADEUS_SECRET_BECOME_PASSWORD') can only ever be fed the row
-- literally named BECOME_PASSWORD. `alias` is the third field that breaks the
-- tie: the DESTINATION name the resolved value is injected under.
--
--     TEAMA_SUDO (agency TeamA) --alias--> AMADEUS_SECRET_BECOME_PASSWORD
--     TEAMB_SUDO (agency TeamB) --alias--> AMADEUS_SECRET_BECOME_PASSWORD
--
-- ⚠️ THE ALIAS IS A DESTINATION, NOT A SELECTOR (plan §2.2). Resolution is
-- unchanged: ref_name still names the row, still resolved by runref.lookupScoped
-- under the run's scope + frozen agency snapshot, still fail-closed. An
-- implementation that resolves BY alias, or lets an alias select a row, inverts
-- the entire least-privilege model. Nothing here touches the resolution predicate.
--
-- WHY THE TABLE IS REBUILT RATHER THAN JUST ALTERed. The alias belongs in the
-- primary key. The same row aliased twice in one owner is legitimate (the plan's
-- RA-4 dedupe key is kind+name+alias), and the old PK
-- (owner_kind, owner_source, owner_name, ref_kind, ref_name) forbids it — the
-- second INSERT would be a constraint violation surfacing as a 500 rather than a
-- supported declaration. SQLite cannot alter a PRIMARY KEY in place, so the table
-- goes through the standard create_new → copy → drop → rename rebuild
-- (migration 490's pattern). The change is a WIDENING — every existing row copies
-- across with alias='' — so it cannot reject data.
--
-- ⚠️ Trigger caveat (the migration-200 rule, in its less obvious form). 590's two
-- cascade triggers are declared ON jobs / ON scripts — NOT on this table — so it is
-- tempting to leave them alone. That fails: SQLite re-parses every trigger body
-- during ALTER TABLE … RENAME, and at that instant `reference_bindings` has been
-- dropped and not yet recreated, so both triggers reference a missing table and the
-- migration aborts. They are therefore dropped up front and recreated verbatim from
-- 590 at the end, together with the owner index.
DROP TRIGGER IF EXISTS reference_bindings_job_cleanup;
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;

CREATE TABLE reference_bindings_new (
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','script')),
    owner_source TEXT NOT NULL DEFAULT '',
    owner_name   TEXT NOT NULL,
    ref_kind     TEXT NOT NULL CHECK (ref_kind IN ('secret','var','key')),
    ref_name     TEXT NOT NULL,
    -- '' = inject under the row's own name (every pre-820 binding, and the
    -- default for every new one). Non-empty = the bare destination name, which
    -- carries the same charset rules as a row name of that kind so the derived
    -- AMADEUS_<SECTION>_<alias> is always a legal env-var key.
    alias        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (owner_kind, owner_source, owner_name, ref_kind, ref_name, alias)
);
INSERT INTO reference_bindings_new
    (owner_kind, owner_source, owner_name, ref_kind, ref_name, alias, created_by, created_at)
    SELECT owner_kind, owner_source, owner_name, ref_kind, ref_name, '', created_by, created_at
    FROM reference_bindings;
DROP TABLE reference_bindings;
ALTER TABLE reference_bindings_new RENAME TO reference_bindings;

-- Recreated verbatim from 590: dispatch resolves a run's bindings by owner, so the
-- owner triple must stay a covering range scan rather than a full-table scan.
CREATE INDEX idx_reference_bindings_owner
    ON reference_bindings (owner_kind, owner_source, owner_name);

-- Recreated verbatim from 590. Without these a deleted job/script leaves an
-- orphaned reference grant that a LATER same-named owner would silently inherit —
-- a least-privilege violation now that injection is live.
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
