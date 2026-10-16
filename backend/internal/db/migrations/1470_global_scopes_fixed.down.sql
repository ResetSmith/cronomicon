-- 1470 down: Global's repository's scopes may be repaired into an agency
-- again (1420's trigger, verbatim). LOSSY: a scope the up made the agency's
-- own stays its own, built in the app, and only loses the mark of where it
-- came from; Global's next sync of its file then makes Global's scope of the
-- name again.

DROP TRIGGER scope_agencies_repo_fixed;
CREATE TRIGGER scope_agencies_repo_fixed BEFORE INSERT ON scope_agencies
WHEN EXISTS (SELECT 1 FROM scopes sc JOIN git_repos g ON g.id = sc.repo_id
              WHERE sc.id = NEW.scope_id AND sc.source = 'git'
                AND g.id <> 'global' AND g.agency_id <> NEW.agency_id)
BEGIN
    SELECT RAISE(ABORT, 'scope_agency_fixed: a scope that comes from an agency''s repository belongs to that agency');
END;

ALTER TABLE scopes DROP COLUMN released_from;
