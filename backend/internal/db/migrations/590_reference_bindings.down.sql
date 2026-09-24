-- Reverse 590. Drop the cleanup triggers FIRST — they live ON jobs/scripts, so
-- dropping reference_bindings would not remove them and they'd dangle against a
-- missing table. Then the owner index and the table itself.
DROP TRIGGER IF EXISTS reference_bindings_job_cleanup;
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;
DROP INDEX IF EXISTS idx_reference_bindings_owner;
DROP TABLE IF EXISTS reference_bindings;
