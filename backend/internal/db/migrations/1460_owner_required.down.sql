-- 1460 down: the triggers go, and the name-keyed index of 1050 comes back
-- (1290's down relies on it). An owner set to Global by the up stays Global's:
-- what it was (NULL or '') owned nothing.
DROP TRIGGER IF EXISTS jobs_owner_required_insert;
DROP TRIGGER IF EXISTS jobs_owner_required;
DROP TRIGGER IF EXISTS workflows_owner_required_insert;
DROP TRIGGER IF EXISTS workflows_owner_required;
DROP TRIGGER IF EXISTS schedules_owner_required_insert;
DROP TRIGGER IF EXISTS schedules_owner_required;
DROP TRIGGER IF EXISTS calendars_owner_required_insert;
DROP TRIGGER IF EXISTS calendars_owner_required;
DROP TRIGGER IF EXISTS scopes_owner_required_insert;
DROP TRIGGER IF EXISTS scopes_owner_required;

CREATE UNIQUE INDEX IF NOT EXISTS uq_reference_bindings_script
    ON reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, alias)
    WHERE owner_uid IS NULL;
