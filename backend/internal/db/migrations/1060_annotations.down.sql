-- Reverse 1060.
--
-- ⚠️ Dropping `annotations` destroys operator-authored content that exists
-- NOWHERE else. Unlike a derived cache or a sync-owned column, nothing
-- re-creates these: the git repository never held them (that is the whole
-- point of the band — they are what git's `description` cannot say), and a
-- re-run of sync will not bring them back. A rollback past this migration
-- loses every "call the DBA distribution list, this one pages at 3am" note in
-- the fleet, silently, and the surfaces that showed them simply render nothing.
--
-- Triggers first: they are ON jobs/workflows, so dropping the table alone would
-- leave two triggers referencing a table that no longer exists, and the next
-- DELETE on either parent would fail at runtime rather than here.
DROP TRIGGER IF EXISTS annotations_workflow_delete;
DROP TRIGGER IF EXISTS annotations_job_delete;
DROP TABLE IF EXISTS annotations;
