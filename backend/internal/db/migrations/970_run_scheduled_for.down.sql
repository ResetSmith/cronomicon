-- Reverses 970. Dropping the column loses the fire-instant evidence for any
-- run promoted out of the queue while it was in place; the detector simply
-- returns to its pre-970 behaviour of missing those, which is the defect this
-- migration exists to fix rather than data anyone can act on.
DROP INDEX IF EXISTS idx_workflow_runs_name_created;
ALTER TABLE runs DROP COLUMN scheduled_for;
