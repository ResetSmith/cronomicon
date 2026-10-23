-- Lossy. activity: 2.4.4's request-stamped agency_id is not restored where the
-- backfill replaced it with the object's. workflow_runs and change_log: an
-- agency a writer named that no key can find again (a refused child run's
-- parent's; a change's object) is Global's after an up.
DROP INDEX IF EXISTS idx_workflow_runs_agency_started;
DROP INDEX IF EXISTS idx_change_log_agency_at;
DROP INDEX IF EXISTS idx_activity_agency_at;
DROP TRIGGER IF EXISTS workflow_runs_agency_stamped;
ALTER TABLE change_log DROP COLUMN agency_id;
ALTER TABLE workflow_runs DROP COLUMN agency_id;
