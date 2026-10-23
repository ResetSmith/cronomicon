DROP INDEX IF EXISTS idx_runs_owner_started;
DROP TRIGGER IF EXISTS runs_owner_stamped;
ALTER TABLE runs DROP COLUMN owner_agency;
