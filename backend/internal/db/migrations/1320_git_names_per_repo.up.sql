-- 1320_git_names_per_repo — a Git job's and a Git workflow's name is unique
-- within its repository, not across all of them (2.4.0, GR-4; Phase R3).
--
-- Since 1050 the two partial unique indexes below said "one Git job, and one
-- Git workflow, of a name", with the stated reason that YAML names live in one
-- repository. 2.4.0 gives each agency a repository, and two repositories will
-- both have a `nightly`. Sync's upsert conflicts on these indexes: keyed on
-- the name alone, the second repository's `nightly` overwrote the first's.
--
-- Nothing else changes here: every Git row is Global's today (migration 1310
-- stamped them), so the new indexes hold exactly the rows the old ones did.
-- A job built in the app is outside both, as before: its name is unique
-- within its agency's pool, which the composer checks.

-- A Git row always names its repository; an index over (repo_id, name) would
-- not see one that did not (NULLs are distinct). 1310 left none, and sync has
-- stamped every row since. This is for a row written in between by an older
-- binary that was left running through the upgrade.
UPDATE jobs      SET repo_id = 'global' WHERE source = 'git' AND repo_id IS NULL;
UPDATE workflows SET repo_id = 'global' WHERE source = 'git' AND repo_id IS NULL;
UPDATE scopes    SET repo_id = 'global' WHERE source = 'git' AND repo_id IS NULL;

DROP INDEX IF EXISTS uq_jobs_git_name;
CREATE UNIQUE INDEX uq_jobs_git_name ON jobs(repo_id, name) WHERE source = 'git';

DROP INDEX IF EXISTS uq_workflows_git_name;
CREATE UNIQUE INDEX uq_workflows_git_name ON workflows(repo_id, name) WHERE source = 'git';

-- ── The log-folder code of a Git definition knows its repository ────────────
--
-- A job's runs are logged under a folder named by its entity code. A Git job
-- that is pruned and later returns is a NEW row with a new uid, and must keep
-- its folder: sync finds the code its predecessor left by ('git', name) and
-- re-points it at the new uid. With one name in two repositories that lookup
-- finds the OTHER repository's code as readily, and the two jobs would take
-- turns owning one folder. The code records the repository it was allocated
-- for, and the re-point looks only among its own.
ALTER TABLE entity_codes ADD COLUMN repo_id TEXT;
UPDATE entity_codes SET repo_id = 'global' WHERE source = 'git';
