-- 1010_run_history_uids — AF-4b stage R2-1: run history and the activity feed
-- carry the definition's permanent uid alongside its name
-- (the rbac2 plan).
--
-- WHY. 1000 gave every job/workflow/schedule a uid; nothing referenced it yet.
-- This is the first referencing edge to move: runs, workflow_runs and activity
-- gain a uid column stamped at write time by every producer, so History stays
-- attributable once AF-4b's final stage lets two agencies own the same name.
-- Behavior-neutral: names stay globally unique per source, every existing
-- textual join is untouched, and the name columns remain (they are the display
-- snapshot that must survive the definition's deletion).
--
-- BACKFILL is by name-join and runs NOW, while (source, name) still uniquely
-- identifies a definition — that window is the whole reason AF-4b is staged.
-- runs/workflow_runs carry a source column (NULL ⇒ git, the 170 convention), so
-- their join is exact. activity has NO source column, and the same name CAN
-- legally exist in both source pools today, so an activity row backfills only
-- when its name is unique across pools and stays NULL otherwise — activity is a
-- display feed, and a NULL uid on an ambiguous historical row costs nothing,
-- while guessing a source would attribute one pool's history to the other.
-- Rows whose definition is gone (deleted before this migration) also stay NULL,
-- exactly like the entity_code column's "no live code" convention.
--
-- The new indexes back the uid-keyed history filters that take over from the
-- name-keyed ones as read paths move.

ALTER TABLE runs          ADD COLUMN job_uid TEXT;
ALTER TABLE workflow_runs ADD COLUMN workflow_uid TEXT;
ALTER TABLE activity      ADD COLUMN job_uid TEXT;
ALTER TABLE activity      ADD COLUMN workflow_uid TEXT;

UPDATE runs SET job_uid =
	(SELECT j.uid FROM jobs j
	  WHERE j.name = runs.job_name
	    AND j.source = COALESCE(runs.job_source, 'git'));

UPDATE workflow_runs SET workflow_uid =
	(SELECT w.uid FROM workflows w
	  WHERE w.name = workflow_runs.workflow_name
	    AND w.source = COALESCE(workflow_runs.workflow_source, 'git'));

UPDATE activity SET job_uid =
	(SELECT j.uid FROM jobs j
	  WHERE j.name = activity.job_name
	    AND (SELECT COUNT(*) FROM jobs j2 WHERE j2.name = j.name) = 1)
	WHERE job_name IS NOT NULL;

UPDATE activity SET workflow_uid =
	(SELECT w.uid FROM workflows w
	  WHERE w.name = activity.workflow_name
	    AND (SELECT COUNT(*) FROM workflows w2 WHERE w2.name = w.name) = 1)
	WHERE workflow_name IS NOT NULL;

CREATE INDEX idx_runs_job_uid_created          ON runs(job_uid, created_at);
CREATE INDEX idx_workflow_runs_uid_created     ON workflow_runs(workflow_uid, created_at);
