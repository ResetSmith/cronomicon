-- 1450_agency_delete_guard — an agency is deleted only when it is empty, and
-- its repository with it (2.4.0 Phase I3; §0a, IN-13).
--
-- The guard of 1250 refused the delete while anything held a membership of
-- the agency or was owned by it, and did not ask about what it had BUILT: a
-- job (owner since 1400), a workflow (1360), a schedule (1300) or a calendar
-- (1390) of the agency was left owned by an agency that no longer existed.
-- Nor about its repository's connection. The route takes the repository
-- (and what it supplied) before it deletes the agency, so by then nothing of
-- these is left; this is what refuses a row that arrived between the two.
DROP TRIGGER agencies_no_delete_in_use;
CREATE TRIGGER agencies_no_delete_in_use BEFORE DELETE ON agencies
WHEN EXISTS (SELECT 1 FROM scope_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM runner_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secret_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM env_var_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credential_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secrets WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM env_vars WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credentials WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_hosts WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM bastions WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM runners WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM jobs WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM workflows WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM schedules WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM calendars WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM git_repos WHERE agency_id = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables, keys, host records, bastions, definitions, calendars or a repository');
END;
