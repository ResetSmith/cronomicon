-- 1530_alert_owner — an alert rule is one agency's (2.5.0 C4, AL-1; IN-6).
--
-- A rule matches only runs of its agency ("all" means all of that agency's);
-- Global's rules match every agency's runs and are a global administrator's,
-- as every rule was until now. So every existing rule is Global's, and keeps
-- matching what it matched.

ALTER TABLE alert_config ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';

CREATE INDEX idx_alert_config_owner ON alert_config(owner_agency);

-- An agency is deleted only when it is empty (1450): its alert rules too, or
-- they would be left owned by an agency that no longer exists.
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
