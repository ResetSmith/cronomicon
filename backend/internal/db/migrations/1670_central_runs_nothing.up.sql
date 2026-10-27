-- 1670_central_runs_nothing — nothing that runs is MOVED into the built-in
-- agency (2.5.3, CA-1, CA-4).
--
-- Central (the agency 'global', Global on an installation from before 2.5.2)
-- is the installation's administration and runs nothing. The app refuses to
-- create a runnable row as Central's, or to move one there (api,
-- failCentralRunsNothing), and refuses to run what Central owns (scheduler,
-- DefinitionIsCentral). These triggers are the database's half, and it is
-- deliberately the NARROW half (the owner's choice, 2026-10-23): a row whose
-- owner is a real agency cannot be changed to Central's.
--
-- What they do not refuse, on purpose:
--  - A row BORN with no owner ('' or NULL) and filled in as Global's by the
--    birth triggers of 1220 and 1400/1420. Tests write such rows past the app
--    (some 1,150 inserts); the app never does, and such a row is refused when it
--    would run. This is the documented limit of CA-4.
--  - scopes: their owner follows scope_agencies through on_join/on_leave
--    (1440), which pass through Global while a setter deletes and re-inserts a
--    membership. A move of a scope into Central is refused by the app.
--  - calendars: Central's calendars are the catalogue agencies take copies of
--    (CA-25); a calendar of Central's is never bound by a run.
--  - the local runner (kind 'server'): Central's while unassigned (CA-9).
--  - a row that is Central's already (an upgrade's leftover): it stays, may be
--    edited, deleted, and moved out.

CREATE TRIGGER secrets_not_made_central BEFORE UPDATE OF owner_agency ON secrets
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a secret is not moved into the built-in administration agency');
END;

CREATE TRIGGER env_vars_not_made_central BEFORE UPDATE OF owner_agency ON env_vars
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a variable is not moved into the built-in administration agency');
END;

CREATE TRIGGER ssh_credentials_not_made_central BEFORE UPDATE OF owner_agency ON ssh_credentials
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: an SSH key is not moved into the built-in administration agency');
END;

CREATE TRIGGER ssh_hosts_not_made_central BEFORE UPDATE OF owner_agency ON ssh_hosts
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a host record is not moved into the built-in administration agency');
END;

CREATE TRIGGER bastions_not_made_central BEFORE UPDATE OF owner_agency ON bastions
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a bastion is not moved into the built-in administration agency');
END;

CREATE TRIGGER runners_not_made_central BEFORE UPDATE OF owner_agency ON runners
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
 AND NEW.kind <> 'server'
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a runner is not moved into the built-in administration agency');
END;

CREATE TRIGGER schedules_not_made_central BEFORE UPDATE OF owner_agency ON schedules
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a schedule is not moved into the built-in administration agency');
END;

CREATE TRIGGER workflows_not_made_central BEFORE UPDATE OF owner_agency ON workflows
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a workflow is not moved into the built-in administration agency');
END;

CREATE TRIGGER jobs_not_made_central BEFORE UPDATE OF owner_agency ON jobs
WHEN NEW.owner_agency = 'global' AND COALESCE(OLD.owner_agency, '') NOT IN ('', 'global')
BEGIN
    SELECT RAISE(ABORT, 'central_runs_nothing: a job is not moved into the built-in administration agency');
END;
