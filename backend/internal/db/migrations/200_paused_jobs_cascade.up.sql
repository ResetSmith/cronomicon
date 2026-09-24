-- 200_paused_jobs_cascade (PP-H9): cascade paused_jobs on job/workflow delete.
--
-- The (source, owner_kind, name) paused_jobs row is a child of a job/workflow
-- definition, but migration 170's AFTER DELETE triggers cascade only
-- definition_schedules — NOT paused_jobs. So a git definition that was paused
-- and then removed from Git is pruned while its paused_jobs row survives; a
-- later same-named re-sync then finds isPaused()=true and silently skips every
-- fire. The in-app compose-delete paths cleaned paused_jobs manually, but the
-- git prune did not — an asymmetry these triggers close for ALL delete paths
-- (git prune, in-app, and any future delete site).
--
-- NOTE (mirrors 170's caveat at lines 16-18): a SQLite table-REBUILD of jobs /
-- workflows / paused_jobs silently DROPS dependent triggers. Any future
-- migration that rebuilds those tables MUST recreate trg_paused_jobs_job_delete
-- and trg_paused_jobs_workflow_delete (alongside the trg_def_schedules_* pair).
--
-- Match on `source` (the paused_jobs PK column), NOT `owner_source` (that is the
-- definition_schedules column name).

CREATE TRIGGER trg_paused_jobs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'job' AND name = OLD.name AND source = OLD.source;
END;

CREATE TRIGGER trg_paused_jobs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM paused_jobs
    WHERE owner_kind = 'workflow' AND name = OLD.name AND source = OLD.source;
END;

-- One-time cleanup of orphans that already accumulated while the bug was live:
-- triggers only prevent FUTURE orphans, so an existing DB may carry git pauses
-- whose definition was pruned long ago. Remove git-source pauses with no
-- matching git definition (amadeus-source pauses are operator-authored and left
-- untouched, consistent with the prune's source guard).
DELETE FROM paused_jobs
WHERE (owner_kind = 'job'      AND source = 'git' AND name NOT IN (SELECT name FROM jobs      WHERE source = 'git'))
   OR (owner_kind = 'workflow' AND source = 'git' AND name NOT IN (SELECT name FROM workflows WHERE source = 'git'));
