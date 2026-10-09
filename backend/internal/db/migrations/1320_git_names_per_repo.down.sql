-- 1320_git_names_per_repo (down) — a Git name is unique across repositories
-- again.
--
-- Lossy only where it must be: if two repositories each supply a job, or a
-- workflow, of one name, only one can keep it. Global's is kept, else the one
-- with the lowest rowid; the others are deleted, and the delete triggers take
-- their schedule entries, reactions, pauses and bindings with them.

DELETE FROM jobs
 WHERE source = 'git'
   AND rowid NOT IN (
       SELECT COALESCE(
                (SELECT MIN(g.rowid) FROM jobs g WHERE g.source = 'git' AND g.name = j.name AND g.repo_id = 'global'),
                MIN(j.rowid))
         FROM jobs j WHERE j.source = 'git' GROUP BY j.name);

DELETE FROM workflows
 WHERE source = 'git'
   AND rowid NOT IN (
       SELECT COALESCE(
                (SELECT MIN(g.rowid) FROM workflows g WHERE g.source = 'git' AND g.name = w.name AND g.repo_id = 'global'),
                MIN(w.rowid))
         FROM workflows w WHERE w.source = 'git' GROUP BY w.name);

DROP INDEX IF EXISTS uq_jobs_git_name;
CREATE UNIQUE INDEX uq_jobs_git_name ON jobs(source, name) WHERE source = 'git';

DROP INDEX IF EXISTS uq_workflows_git_name;
CREATE UNIQUE INDEX uq_workflows_git_name ON workflows(source, name) WHERE source = 'git';

ALTER TABLE entity_codes DROP COLUMN repo_id;
