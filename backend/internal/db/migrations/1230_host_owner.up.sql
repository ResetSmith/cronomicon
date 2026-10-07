-- 1230_host_owner — a hand-written host record and every bastion belong to an
-- agency (LR-69, LR-70, LR-72; the LR band, v2.3.0).
--
-- A host record comes from one of two places. One IMPORTED for a scope
-- (ssh_hosts.scope_id set) is that scope's, and has been edited under the
-- scope's gate since 2.2.2. One written BY HAND has no scope: it applied to
-- every scope, won over an imported record of the same name, and so could only
-- be a global administrator's. A bastion was the same. That left an agency with
-- no way to describe its own jump host, or to correct one of its own hosts
-- without a global administrator.
--
-- Both get an owner. Existing hand-written records and bastions are Global's,
-- which is exactly what they were: they still apply to every agency's scopes,
-- and only a global administrator changes them. A record an agency writes from
-- now on is seen only by that agency's scopes (execspec.HostByName), may route
-- only through its own bastions or Global's, and may name only its own keys or
-- Global's.
--
-- owner_agency is the agency's id and is never empty. On an imported row it is
-- not consulted — the scope decides — and stays at its default.
ALTER TABLE ssh_hosts ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';
ALTER TABLE bastions  ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';

CREATE INDEX idx_ssh_hosts_owner ON ssh_hosts (owner_agency);
CREATE INDEX idx_bastions_owner  ON bastions (owner_agency);

CREATE TRIGGER ssh_hosts_owner_required_insert BEFORE INSERT ON ssh_hosts WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a host record is owned by an agency, Global at the least');
END;
CREATE TRIGGER ssh_hosts_owner_required BEFORE UPDATE OF owner_agency ON ssh_hosts WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a host record is owned by an agency, Global at the least');
END;
CREATE TRIGGER bastions_owner_required_insert BEFORE INSERT ON bastions WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a bastion is owned by an agency, Global at the least');
END;
CREATE TRIGGER bastions_owner_required BEFORE UPDATE OF owner_agency ON bastions WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a bastion is owned by an agency, Global at the least');
END;

-- An agency that still owns a host record or a bastion cannot be deleted, for
-- the reason 1220 gives for its secrets: the owner would name nothing. The
-- trigger is replaced, not added to, so the rule stays one statement.
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
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables, keys, host records or bastions');
END;
