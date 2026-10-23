-- 1230_host_owner (down). LOSSY: a host record or bastion an agency wrote goes
-- back to having no owner, which under the previous binary means it applies to
-- every scope and only a global administrator may change it.
DROP TRIGGER IF EXISTS agencies_no_delete_in_use;
CREATE TRIGGER agencies_no_delete_in_use BEFORE DELETE ON agencies
WHEN EXISTS (SELECT 1 FROM scope_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM runner_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secret_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM env_var_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credential_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secrets WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM env_vars WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credentials WHERE owner_agency = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables or keys');
END;

DROP TRIGGER IF EXISTS bastions_owner_required;
DROP TRIGGER IF EXISTS bastions_owner_required_insert;
DROP TRIGGER IF EXISTS ssh_hosts_owner_required;
DROP TRIGGER IF EXISTS ssh_hosts_owner_required_insert;
DROP INDEX IF EXISTS idx_bastions_owner;
DROP INDEX IF EXISTS idx_ssh_hosts_owner;
ALTER TABLE bastions  DROP COLUMN owner_agency;
ALTER TABLE ssh_hosts DROP COLUMN owner_agency;
