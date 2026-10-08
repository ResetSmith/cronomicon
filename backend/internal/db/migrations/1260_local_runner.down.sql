-- 1260_local_runner (down). LOSSY: the local runner's row goes, with its serve
-- list. Scope bindings that name it stay (a binding outlives its runner, by
-- design), so those scopes are closed until they are re-bound; the previous
-- binary runs shell jobs from the server by its own switch, as it did.
DELETE FROM runner_agencies WHERE runner_id IN (SELECT id FROM runners WHERE kind = 'server');
DELETE FROM runners WHERE kind = 'server';
DELETE FROM settings WHERE key = 'localRunner.enabled';

-- A list that mixed Global with other agencies was legal only for the local
-- runner, which is gone; nothing else holds one.
DROP TRIGGER IF EXISTS runner_agencies_leave_global;
CREATE TRIGGER runner_agencies_leave_global AFTER INSERT ON runner_agencies WHEN NEW.agency_id <> 'global'
BEGIN
    DELETE FROM runner_agencies WHERE runner_id = NEW.runner_id AND agency_id = 'global';
END;
DROP TRIGGER IF EXISTS runner_agencies_no_mix;
CREATE TRIGGER runner_agencies_no_mix BEFORE INSERT ON runner_agencies
WHEN NEW.agency_id = 'global'
 AND EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = NEW.runner_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: a runner cannot serve Global and another agency');
END;

DROP INDEX IF EXISTS ux_runners_one_server;
ALTER TABLE runners DROP COLUMN kind;
