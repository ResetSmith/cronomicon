-- 1470_global_scopes_fixed — a scope from Git belongs to its repository's
-- agency, Global's repository included (2.4.3; the owner's answer to question
-- 58, option A).
--
-- 1350 and 1420 refused to put a scope of an agency's repository in another
-- agency (scope_agencies_repo_fixed, 409 scope_agency_fixed), and let Global's
-- repository through: a global administrator's repair could give one of
-- Global's repository's scopes to an agency. The scope stayed Global's file's:
-- Global's sync went on writing its hosts, removing the file from Global's
-- repository pruned it out of the agency, and the agency could not edit it (a
-- Git scope is changed in Git). Since §0a nothing of Global's is another
-- agency's (IN-3), and an agency gets another's hosts by a clone (IN-7).
--
-- Now the trigger asks the same of every repository. A scope repaired that way
-- before this migration is the agency's own, built in the app: what a clone of
-- it would be, in place (no file, no repository, its host records the
-- agency's), keeping its hosts, groups, variables and inventory.
-- released_from names the repository it came from, so that the repository's
-- next sync does not make a second scope of the name for Global, and says so:
-- the file is no longer any scope's, and is to be removed or renamed. The
-- column goes with the row: once the agency deletes its scope, Global's file
-- makes Global's scope again.

ALTER TABLE scopes ADD COLUMN released_from TEXT;

UPDATE ssh_hosts
   SET source = 'cronomicon',
       owner_agency = (SELECT sc.owner_agency FROM scopes sc WHERE sc.id = ssh_hosts.scope_id)
 WHERE source = 'git'
   AND scope_id IN (SELECT id FROM scopes
                     WHERE source = 'git' AND COALESCE(repo_id, 'global') = 'global'
                       AND owner_agency <> 'global');

UPDATE scopes
   SET source = 'cronomicon', repo_id = NULL, source_path = NULL, git_meta_json = NULL,
       synced_at = NULL, released_from = 'global'
 WHERE source = 'git' AND COALESCE(repo_id, 'global') = 'global' AND owner_agency <> 'global';

DROP TRIGGER scope_agencies_repo_fixed;
CREATE TRIGGER scope_agencies_repo_fixed BEFORE INSERT ON scope_agencies
WHEN EXISTS (SELECT 1 FROM scopes sc JOIN git_repos g ON g.id = sc.repo_id
              WHERE sc.id = NEW.scope_id AND sc.source = 'git'
                AND g.agency_id <> NEW.agency_id)
BEGIN
    SELECT RAISE(ABORT, 'scope_agency_fixed: a scope that comes from a repository belongs to that repository''s agency; another agency gets its hosts by a clone');
END;
