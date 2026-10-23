-- 1660_global_renamable (down). Back to 1220's rules: the built-in agency is
-- named "Global" again (a renamed one is renamed back, on waiting runs too, as
-- settings.UpdateAgency carries a rename), the name is reserved, and the
-- trigger stamps the literal.
UPDATE runs SET agencies_json = '["Global"]'
 WHERE status IN ('queued', 'running')
   AND agencies_json = json_array((SELECT name FROM agencies WHERE id = 'global'))
   AND (SELECT name FROM agencies WHERE id = 'global') <> 'Global';
UPDATE run_agencies SET agency = 'Global'
 WHERE agency = (SELECT name FROM agencies WHERE id = 'global')
   AND agency <> 'Global'
   AND run_id IN (SELECT id FROM runs WHERE status IN ('queued', 'running'));
DROP TRIGGER IF EXISTS agencies_builtin_stays_builtin;
UPDATE agencies SET name = 'Global' WHERE id = 'global';

CREATE TRIGGER agencies_builtin_no_rename BEFORE UPDATE OF name, builtin ON agencies
WHEN OLD.builtin = 1 AND (NEW.name <> OLD.name OR NEW.builtin <> 1)
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the Global agency cannot be renamed');
END;
CREATE TRIGGER agencies_global_name_reserved BEFORE INSERT ON agencies
WHEN lower(NEW.name) = 'global' AND NEW.id <> 'global'
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the name Global is reserved');
END;
CREATE TRIGGER agencies_global_name_reserved_update BEFORE UPDATE OF name ON agencies
WHEN lower(NEW.name) = 'global' AND OLD.id <> 'global'
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the name Global is reserved');
END;

DROP TRIGGER IF EXISTS runs_born_global;
CREATE TRIGGER runs_born_global AFTER INSERT ON runs
WHEN COALESCE(NEW.agencies_json, '') IN ('', '[]') AND NEW.status IN ('queued', 'running')
BEGIN
    UPDATE runs SET agencies_json = '["Global"]' WHERE id = NEW.id;
    INSERT OR IGNORE INTO run_agencies (run_id, agency) VALUES (NEW.id, 'Global');
END;
