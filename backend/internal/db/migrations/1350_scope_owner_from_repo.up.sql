-- 1350_scope_owner_from_repo — a scope that arrives from an agency's
-- repository is that agency's, and stays so (2.4.0, GR-18; Phase R4).
--
-- A scope synced from Git has been born Global's and assigned to an agency
-- once, by a global administrator (LR-31): the agency is an operator's fact
-- that the one repository could not state. A repository that belongs to an
-- agency states it. The operator said whose it is by connecting it.
--
-- Two triggers, so that the rule holds at every writer and not only at the
-- ones that remember it:
--
--   * BIRTH. A new scope is given the agency of the repository it comes from.
--     Global's repository's scopes, and scopes built in the app, are born
--     Global's as before. Sync still writes no membership row; the database
--     does, once, when the scope's row is inserted. A scope that sync updates
--     is not touched: an assignment an operator made to one of Global's
--     repository's scopes survives every later sync, as it always has.
--
--   * NO MOVE. A scope that is in an agency's repository cannot be given any
--     other agency. It leaves its agency when its file leaves the repository
--     (the scope is then pruned), or when the repository is disconnected.
--
-- No row is rewritten here. Until this release there was no way to connect a
-- second repository, so no scope of one exists.

DROP TRIGGER scopes_born_global;
CREATE TRIGGER scopes_born_global AFTER INSERT ON scopes
BEGIN
    INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id)
    VALUES (NEW.id, COALESCE(
        (SELECT g.agency_id FROM git_repos g WHERE NEW.source = 'git' AND g.id = NEW.repo_id),
        'global'));
END;

CREATE TRIGGER scope_agencies_repo_fixed BEFORE INSERT ON scope_agencies
WHEN EXISTS (SELECT 1 FROM scopes sc JOIN git_repos g ON g.id = sc.repo_id
              WHERE sc.id = NEW.scope_id AND sc.source = 'git'
                AND g.id <> 'global' AND g.agency_id <> NEW.agency_id)
BEGIN
    SELECT RAISE(ABORT, 'scope_agency_fixed: a scope that comes from an agency''s repository belongs to that agency');
END;
