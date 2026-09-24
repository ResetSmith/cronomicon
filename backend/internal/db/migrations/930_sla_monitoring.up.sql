-- 930_sla_monitoring — deadlines, the warn stamp, and the two indexes the new
-- scans need (SL, the prod-features plan §3).
--
-- WHY THESE ARE NOT timeout_seconds. timeout_seconds already exists and is
-- already enforced — in THREE places (sshexec's context deadline, the runner
-- agent's own, and the workflow engine's wait bound). But killing a job is a
-- different question from noticing one. "The nightly backup is still running at
-- 6am" needs a human, not a SIGKILL: the run may well be fine and merely slow,
-- and killing it could be the more destructive answer.
--
-- The consequence is that these cannot be enforced the way timeout_seconds is.
-- All three of its enforcement points are IN-PROCESS context deadlines held by
-- whichever executor owns the run — and for a runner-executed run the server
-- holds no goroutine at all, so there is nothing to hang a deadline on. A soft
-- warning has to be a server-side scan over `runs`, which also means it survives
-- a restart, which an in-memory timer would not.
--
-- Additive: two nullable columns on jobs, one on runs, two indexes. No rebuild.

-- warn_after_seconds: warn once a run has been going this long. Relative, so it
-- suits "this usually takes 20 minutes".
ALTER TABLE jobs ADD COLUMN warn_after_seconds INTEGER;

-- must_finish_by: a wall-clock deadline as 'HH:MM' in the APPLICATION timezone,
-- for the other half of the question — "the nightly close has to be done before
-- the business opens", which no duration expresses, because the run's start time
-- is exactly what varies.
--
-- Stored as text rather than minutes-past-midnight so the stored value reads the
-- way the operator typed it; the scanner resolves it against the app zone, never
-- UTC, for the same reason the calendar gate computes its day there (CAL-7).
ALTER TABLE jobs ADD COLUMN must_finish_by TEXT;

-- sla_warned_at makes the warning fire ONCE per run. Without it a scan every
-- minute would page every minute for as long as the run stayed late, which is
-- how an alerting feature teaches people to filter it into a folder.
ALTER TABLE runs ADD COLUMN sla_warned_at TEXT;

-- The SLA scanner's hot query: "which running runs started before X". Nothing
-- indexed it — including the ssh orphan reaper (sshexec.go), which has been
-- running exactly this shape unindexed every minute since it landed, so this
-- pays for itself twice.
CREATE INDEX idx_runs_running_started ON runs(started_at) WHERE status = 'running';

-- The per-job analytics window: "this job's runs since T". idx_runs_job_name
-- exists but covers only the name, so the date range degrades to a filter over
-- every run the job has ever had — fine at a few hundred, not at a year of a
-- five-minute cron.
CREATE INDEX idx_runs_job_created ON runs(job_name, job_source, created_at);
