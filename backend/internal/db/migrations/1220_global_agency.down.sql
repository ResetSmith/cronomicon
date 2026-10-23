-- 1220_global_agency (down). LOSSY, by nature.
--
-- It removes what the up migration added and cannot restore what the up
-- migration changed on the strength of data that has since moved:
--
--   * an unowned secret, variable or key that was given its one member agency as
--     its owner KEEPS that owner (the down cannot tell it from a row created
--     owned);
--   * everything created in Global since the upgrade goes back to "no agency",
--     which under the previous binary means general pool / shared tier — the same
--     thing, by the old convention;
--   * an agency the upgrade renamed away from "Global" keeps its new name;
--   * the notices table goes, with every notice in it.
--
-- The supported way back from 2.3.0 is the database snapshot taken before the
-- first boot. This exists so the schema round-trips.

DROP TRIGGER IF EXISTS ssh_credentials_owner_required;
DROP TRIGGER IF EXISTS env_vars_owner_required;
DROP TRIGGER IF EXISTS secrets_owner_required;
DROP TRIGGER IF EXISTS agencies_no_delete_in_use;
DROP TRIGGER IF EXISTS agencies_global_name_reserved_update;
DROP TRIGGER IF EXISTS agencies_global_name_reserved;
DROP TRIGGER IF EXISTS agencies_builtin_no_rename;
DROP TRIGGER IF EXISTS agencies_builtin_no_delete;
DROP TRIGGER IF EXISTS ssh_credential_agencies_no_mix;
DROP TRIGGER IF EXISTS ssh_credential_agencies_leave_global;
DROP TRIGGER IF EXISTS env_var_agencies_no_mix;
DROP TRIGGER IF EXISTS env_var_agencies_leave_global;
DROP TRIGGER IF EXISTS secret_agencies_no_mix;
DROP TRIGGER IF EXISTS secret_agencies_leave_global;
DROP TRIGGER IF EXISTS runner_agencies_no_mix;
DROP TRIGGER IF EXISTS runner_agencies_leave_global;
DROP TRIGGER IF EXISTS scope_agencies_no_mix;
DROP TRIGGER IF EXISTS scope_agencies_leave_global;
DROP TRIGGER IF EXISTS runs_born_global;
DROP TRIGGER IF EXISTS ssh_credentials_born_global;
DROP TRIGGER IF EXISTS env_vars_born_global;
DROP TRIGGER IF EXISTS secrets_born_global;
DROP TRIGGER IF EXISTS runners_born_global;
DROP TRIGGER IF EXISTS scopes_born_global;

-- Waiting runs stamped Global go back to the empty snapshot.
DELETE FROM run_agencies
 WHERE agency = 'Global' AND run_id IN (SELECT id FROM runs WHERE agencies_json = '["Global"]');
UPDATE runs SET agencies_json = '[]' WHERE agencies_json = '["Global"]';

-- Ownership: Global-owned goes back to unowned.
UPDATE secrets         SET owner_agency = '' WHERE owner_agency = 'global';
UPDATE env_vars        SET owner_agency = '' WHERE owner_agency = 'global';
UPDATE ssh_credentials SET owner_agency = '' WHERE owner_agency = 'global';

-- Membership: the Global rows go (each cascades from the agency row too; said
-- explicitly so the intent survives a future change to the foreign keys).
DELETE FROM scope_agencies          WHERE agency_id = 'global';
DELETE FROM runner_agencies         WHERE agency_id = 'global';
DELETE FROM secret_agencies         WHERE agency_id = 'global';
DELETE FROM env_var_agencies        WHERE agency_id = 'global';
DELETE FROM ssh_credential_agencies WHERE agency_id = 'global';

DELETE FROM agencies WHERE id = 'global';

DROP INDEX IF EXISTS idx_notices_open;
DROP TABLE IF EXISTS notices;

ALTER TABLE agencies DROP COLUMN builtin;
