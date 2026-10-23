-- 1560_agency_concurrency — a cap per agency (2.5.0 C6, QT-1; question 17).
--
-- The installation has one concurrency cap (settings.maxConcurrent). Until
-- 2.5.0 it counted every agency's waiting runs as well as the running ones,
-- and nothing ends a run that stays queued: one agency's switched-off agent
-- left a waiting run at every fire and, at the cap, every agency's fires were
-- skipped (present defect 47). Now a waiting run counts only against fires of
-- its own agency, running runs count against everyone, and the cap is asked at
-- the claim as well as where a run is produced (internal/runcap).
--
-- An agency may also have a cap of its own, set by a global administrator:
-- its runs, waiting and running, at a fire; its running runs at the claim.
-- NULL is none, and the installation's cap still binds.
ALTER TABLE agencies ADD COLUMN max_concurrent INTEGER CHECK (max_concurrent IS NULL OR max_concurrent > 0);

-- The claim counts an agency's running runs (runcap.ClaimSQL) on every poll.
-- Only running runs are indexed, so the count reads the few that are running
-- and not every run the agency has had.
CREATE INDEX idx_runs_running_agency ON runs(owner_agency) WHERE status = 'running';
