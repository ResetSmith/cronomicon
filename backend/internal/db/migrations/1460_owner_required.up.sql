-- 1460_owner_required — an owner column is never emptied (2.4.2; present
-- defect 67, found by the review of 2.4.x).
--
-- 1220 refused an empty owner on secrets, variables and keys, 1230 on host
-- records and bastions, 1250 on runners. The owner columns 2.4.0 added were
-- not given the same: jobs.owner_agency (1400) is nullable, and schedules
-- (1300), workflows (1360), calendars (1390) and scopes (1410) are NOT NULL
-- but take ''. A row with no owner is seen by no agency's reads, counted by
-- no delete guard (1450 asks owner_agency = the agency), and run as nobody's.
-- No writer empties one today; these make it impossible.
--
-- A job is still written with NULL and given its owner by jobs_born_owned
-- (1400, 1420), which runs AFTER the insert: so NULL is refused on an update
-- only, and '' on both.
--
-- What an older binary left empty is Global's (as every backfill since 1220
-- decided for a row with no owner). None is expected: no writer could leave
-- one.
UPDATE jobs      SET owner_agency = 'global' WHERE owner_agency IS NULL OR owner_agency = '';
UPDATE workflows SET owner_agency = 'global' WHERE owner_agency = '';
UPDATE schedules SET owner_agency = 'global' WHERE owner_agency = '';
UPDATE calendars SET owner_agency = 'global' WHERE owner_agency = '';
UPDATE scopes    SET owner_agency = 'global' WHERE owner_agency = '';

CREATE TRIGGER jobs_owner_required_insert BEFORE INSERT ON jobs WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a job is owned by an agency, Global at the least');
END;
CREATE TRIGGER jobs_owner_required BEFORE UPDATE OF owner_agency ON jobs
WHEN NEW.owner_agency IS NULL OR NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a job is owned by an agency, Global at the least');
END;

CREATE TRIGGER workflows_owner_required_insert BEFORE INSERT ON workflows WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a workflow is owned by an agency, Global at the least');
END;
CREATE TRIGGER workflows_owner_required BEFORE UPDATE OF owner_agency ON workflows WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a workflow is owned by an agency, Global at the least');
END;

CREATE TRIGGER schedules_owner_required_insert BEFORE INSERT ON schedules WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a schedule is owned by an agency, Global at the least');
END;
CREATE TRIGGER schedules_owner_required BEFORE UPDATE OF owner_agency ON schedules WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a schedule is owned by an agency, Global at the least');
END;

CREATE TRIGGER calendars_owner_required_insert BEFORE INSERT ON calendars WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a calendar is owned by an agency, Global at the least');
END;
CREATE TRIGGER calendars_owner_required BEFORE UPDATE OF owner_agency ON calendars WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a calendar is owned by an agency, Global at the least');
END;

CREATE TRIGGER scopes_owner_required_insert BEFORE INSERT ON scopes WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a scope is owned by an agency, Global at the least');
END;
CREATE TRIGGER scopes_owner_required BEFORE UPDATE OF owner_agency ON scopes WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a scope is owned by an agency, Global at the least');
END;

-- A script's reference bindings carry their owner's uid since 1290, and are
-- kept unique by uq_reference_bindings_job (whose name predates that). The
-- name-keyed index for bindings with no owner uid has nothing left to keep.
DROP INDEX IF EXISTS uq_reference_bindings_script;
