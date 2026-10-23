-- 1410_scope_owner — a scope's agency as a column (2.4.0, §0a, IN-5, IN-6;
-- Phase I2).
--
-- A scope's agencies are rows of scope_agencies (670), and a scope from before
-- 2.3.0 can be in several. Once a scope's name is unique within an agency
-- only (I2), every lookup of a scope by name asks "this name, in this
-- agency", and the database must be able to hold two scopes of one name in
-- two agencies and refuse two in one. That needs the agency on the row.
--
-- owner_agency is the scope's one agency when it has exactly one, and Global's
-- otherwise: a scope in several agencies (the state from before 2.3.0, listed
-- by scope_several_agencies) is Global's, as its jobs and runs already are
-- (agencyid.DefinitionOf until 1400; ScopeOutsideAgency refuses an agency's
-- job on it). scope_agencies stays what says it; the triggers below keep the
-- column equal to it, whichever way the membership is written.
--
-- The name stays unique across the installation until the next migration
-- rebuilds the table.

ALTER TABLE scopes ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';

UPDATE scopes SET owner_agency = COALESCE(
    (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = scopes.id),
    'global');

CREATE TRIGGER scope_owner_on_join AFTER INSERT ON scope_agencies
BEGIN
    UPDATE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = NEW.scope_id),
        'global')
     WHERE id = NEW.scope_id;
END;

CREATE TRIGGER scope_owner_on_leave AFTER DELETE ON scope_agencies
BEGIN
    UPDATE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = OLD.scope_id),
        'global')
     WHERE id = OLD.scope_id;
END;

CREATE INDEX idx_scopes_owner_name ON scopes (owner_agency, name);
