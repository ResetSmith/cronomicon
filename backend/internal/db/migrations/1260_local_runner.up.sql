-- 1260_local_runner — a runner has a kind (LR-38, LR-39, MA-14; the LR band,
-- v2.3.0).
--
-- Every runner until now was an AGENT: a separate process that registers with
-- a token, polls for work and reports back. The server has also always been
-- able to run shell jobs itself, over SSH, from inside its own process — the
-- "in-app SSH executor", which was not a runner at all: it had no row, claimed
-- with its own query, and served every agency.
--
-- That engine becomes a runner like the others: the LOCAL RUNNER, one row of
-- kind `server`. This migration adds the kind and what the kind changes in the
-- schema's own rules. The row itself is written by the server at boot
-- (settings.EnsureLocalRunner), not here: its concurrency and its first on/off
-- state are seeded from the environment, which SQL cannot read, and its id is
-- an ordinary runner id. Bindings, placements and ledger rows key on that id,
-- so the row is never deleted; "off" is a status, not an absence.
--
-- The value is `server`, not `local`: runners.inventory already has a `local`
-- (the agent resolves hosts against its own inventory) and the Runners view
-- already badges it.
ALTER TABLE runners ADD COLUMN kind TEXT NOT NULL DEFAULT 'agent' CHECK (kind IN ('agent', 'server'));

-- At most one local runner.
CREATE UNIQUE INDEX ux_runners_one_server ON runners (kind) WHERE kind = 'server';

-- The local runner is the ONE runner with a serve list (MA-11, MA-14): any
-- non-empty set of agencies a global administrator gives it, which may include
-- Global beside others. The two triggers of 1220 that keep Global apart from
-- every other agency on a runner's list therefore stop applying to it. They
-- are unchanged for agents, whose list is exactly their owner anyway.
DROP TRIGGER runner_agencies_leave_global;
CREATE TRIGGER runner_agencies_leave_global AFTER INSERT ON runner_agencies
WHEN NEW.agency_id <> 'global'
 AND COALESCE((SELECT kind FROM runners WHERE id = NEW.runner_id), 'agent') <> 'server'
BEGIN
    DELETE FROM runner_agencies WHERE runner_id = NEW.runner_id AND agency_id = 'global';
END;

DROP TRIGGER runner_agencies_no_mix;
CREATE TRIGGER runner_agencies_no_mix BEFORE INSERT ON runner_agencies
WHEN NEW.agency_id = 'global'
 AND COALESCE((SELECT kind FROM runners WHERE id = NEW.runner_id), 'agent') <> 'server'
 AND EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = NEW.runner_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: a runner cannot serve Global and another agency');
END;
