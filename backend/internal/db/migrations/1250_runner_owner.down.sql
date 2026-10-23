-- 1250_runner_owner (down). LOSSY: an agent an agency enrolled keeps serving
-- that agency (its serve row stays), but who owns it is forgotten, and under the
-- previous binary it is administered by whoever administers an agency it serves.
-- An unused token minted for an agency is revoked (see below).
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
  OR EXISTS (SELECT 1 FROM ssh_hosts WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM bastions WHERE owner_agency = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables, keys, host records or bastions');
END;

DROP INDEX IF EXISTS idx_runner_tokens_runner;
-- A token an agency's administrator minted for their agency must not survive as
-- a token for an unplaced runner: under the previous binary that is a
-- general-pool enrolment, which only a global administrator could grant. Unused
-- ones are revoked; mint again after the rollback.
UPDATE registration_tokens
   SET revoked_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
 WHERE agency_id <> 'global' AND used_at IS NULL AND revoked_at IS NULL;
ALTER TABLE registration_tokens DROP COLUMN agency_id;
ALTER TABLE runner_placement_history DROP COLUMN owner_agency;

DROP TRIGGER IF EXISTS runners_born_serving_owner;
CREATE TRIGGER runners_born_global AFTER INSERT ON runners
BEGIN
    INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (NEW.id, 'global');
END;

DROP TRIGGER IF EXISTS runners_owner_required;
DROP TRIGGER IF EXISTS runners_owner_required_insert;
DROP INDEX IF EXISTS idx_runners_owner;
ALTER TABLE runners DROP COLUMN owner_agency;
