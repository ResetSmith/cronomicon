-- Lossy: an agency's targets are dropped, and every rule pushes to every
-- target of the old list again (it was left as it was at the up migration;
-- Global's targets changed since are not written back to it).
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
  OR EXISTS (SELECT 1 FROM alert_config WHERE owner_agency = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables, keys, host records, bastions, definitions, calendars, a repository or alert rules');
END;
DROP TABLE alert_rule_targets;
DROP TABLE alert_targets;
