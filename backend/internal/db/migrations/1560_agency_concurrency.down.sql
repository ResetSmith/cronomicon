-- 1560_agency_concurrency (down). The per-agency caps go. Under the previous
-- binary the installation's cap counts every agency's waiting runs again.
DROP INDEX IF EXISTS idx_runs_running_agency;
ALTER TABLE agencies DROP COLUMN max_concurrent;
