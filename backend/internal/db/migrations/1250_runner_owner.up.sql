-- 1250_runner_owner — a runner has an owner (LR-58, LR-61, LR-64, MA-1, MA-9;
-- the LR band, v2.3.0).
--
-- Until now a runner had only a serve list (runner_agencies): the agencies
-- whose runs it claims. Who ADMINISTERED it was "anyone who administers any
-- agency on that list", and who could enrol one was a global administrator
-- alone, into no agency, to be placed afterwards.
--
-- A runner now has one owner. For an AGENT (every runner there is today) the
-- owner is the agency it serves, and that is the whole of its placement: it is
-- set by the registration token it enrolled with (registration_tokens.agency_id)
-- and does not change, except that a Global-owned agent serving one agency may
-- be handed to that agency. An agency's administrators enrol and manage their
-- own agents. Nobody can make an agent that serves two agencies.
--
-- THE UPGRADE (LR-64), by what a runner serves today:
--
--   one agency          owned by that agency. Nothing else changes.
--   Global only         owned by Global. Nothing changes.
--   several agencies    owned by Global, serve list UNCHANGED: a "legacy
--                       placement". It claims the same runs with the same keys
--                       and toolchains. What changes is who administers it: any
--                       member agency's administrators until now, a global
--                       administrator from now on. It can be narrowed and never
--                       widened, and the inbox lists it (legacy_placement).
--
-- owner_agency has a DEFAULT of 'global' on purpose: sixty hand-written runner
-- fixtures and the registration INSERT of an older path would otherwise all
-- need the column, and Global-owned-serving-Global is exactly what a row with no
-- other information is. The rule that the serve list matches the owner is the
-- writers' (settings.CheckRunnerPlacement), not a constraint: a legacy
-- placement is a supported state that must be able to exist.
ALTER TABLE runners ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';

UPDATE runners
   SET owner_agency = (SELECT MIN(ra.agency_id) FROM runner_agencies ra WHERE ra.runner_id = runners.id)
 WHERE (SELECT COUNT(*) FROM runner_agencies ra WHERE ra.runner_id = runners.id) = 1;

CREATE INDEX idx_runners_owner ON runners (owner_agency);

CREATE TRIGGER runners_owner_required_insert BEFORE INSERT ON runners WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a runner is owned by an agency, Global at the least');
END;
CREATE TRIGGER runners_owner_required BEFORE UPDATE OF owner_agency ON runners WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a runner is owned by an agency, Global at the least');
END;

-- A runner is born serving its owner. 1220's trigger gave every new runner a
-- Global row; an agent enrolled by an agency must never serve Global, even for
-- the instant before its own row is written, so the trigger is replaced by one
-- that reads the owner.
DROP TRIGGER runners_born_global;
CREATE TRIGGER runners_born_serving_owner AFTER INSERT ON runners
BEGIN
    INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (NEW.id, NEW.owner_agency);
END;

-- The snapshot taken when a runner is deregistered keeps its owner, so a
-- restore can tell whose placement it was (MA-32). Old snapshots get the same
-- mapping as live runners: one agency, that agency; otherwise Global.
ALTER TABLE runner_placement_history ADD COLUMN owner_agency TEXT NOT NULL DEFAULT 'global';
UPDATE runner_placement_history
   SET owner_agency = json_extract(agency_ids, '$[0]')
 WHERE json_valid(agency_ids) AND json_type(agency_ids) = 'array' AND json_array_length(agency_ids) = 1
   AND json_type(agency_ids, '$[0]') = 'text' AND json_extract(agency_ids, '$[0]') <> '';

-- A registration token names the owner of the agent it will enrol (LR-32,
-- LR-61). It is the operator's grant, held server-side: the agent sends nothing
-- new and still never declares its own placement. Existing tokens enrol what
-- they always would have: a Global-owned agent serving Global.
ALTER TABLE registration_tokens ADD COLUMN agency_id TEXT NOT NULL DEFAULT 'global';

-- A runner's API keys are revoked by the runner's id (the names are
-- self-declared and not unique). The column has existed since migration 130.
CREATE INDEX idx_runner_tokens_runner ON runner_tokens (runner_id);

-- An agency that owns a runner cannot be deleted. (A runner it serves already
-- blocks the delete; this covers an owner that is not on its own serve list,
-- which no writer produces and the trigger should not depend on.)
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
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables, keys, host records or bastions');
END;
