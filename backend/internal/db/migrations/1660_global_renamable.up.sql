-- 1660_global_renamable (2.5.2 GR; decided 2026-10-21, planned in
-- .aidata/20261006-2.5.1.md, "Global becomes renamable").
--
-- The built-in agency keeps its id ('global'), stays built in and cannot be
-- deleted; its NAME may change, by a global administrator through the rename
-- path every agency uses (settings.UpdateAgency, which carries the new name
-- onto waiting runs). A new installation names it "Central" at its first start
-- (settings.NameNewInstallationAgency); an existing one keeps "Global" until it
-- is renamed. Migration 1220's insert of "Global" stays as it is: a migration is
-- history.
--
-- Runs store their agencies by NAME and the claim matches those names against
-- the live catalog, so the trigger that gives a waiting run with no snapshot to
-- the built-in agency stamps its CURRENT name, not the literal: a literal left
-- behind would make such runs claimable by nobody after a rename.

-- Renaming is allowed; un-marking the built-in agency is not.
DROP TRIGGER IF EXISTS agencies_builtin_no_rename;
CREATE TRIGGER agencies_builtin_stays_builtin BEFORE UPDATE OF builtin ON agencies
WHEN OLD.builtin = 1 AND NEW.builtin <> 1
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the built-in agency stays built in');
END;

-- The name is no longer reserved: any agency name is unique (the UNIQUE
-- constraint on agencies.name), and that is the only rule left.
DROP TRIGGER IF EXISTS agencies_global_name_reserved;
DROP TRIGGER IF EXISTS agencies_global_name_reserved_update;

-- A waiting run with no snapshot is the built-in agency's, by its name now.
DROP TRIGGER IF EXISTS runs_born_global;
CREATE TRIGGER runs_born_global AFTER INSERT ON runs
WHEN COALESCE(NEW.agencies_json, '') IN ('', '[]') AND NEW.status IN ('queued', 'running')
BEGIN
    UPDATE runs SET agencies_json = json_array((SELECT name FROM agencies WHERE id = 'global')) WHERE id = NEW.id;
    INSERT OR IGNORE INTO run_agencies (run_id, agency) VALUES (NEW.id, (SELECT name FROM agencies WHERE id = 'global'));
END;
