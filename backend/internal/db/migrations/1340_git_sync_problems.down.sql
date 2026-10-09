-- 1340_git_sync_problems (down). The rows are what the last sync said; the
-- next sync says it again, in the history row and the log, as before.
DROP INDEX IF EXISTS idx_git_sync_problems_repo;
DROP TABLE git_sync_problems;
-- The inbox notice of a repository that had problems is of a kind the older
-- code does not write or resolve: it goes with the table.
DELETE FROM notices WHERE kind = 'git_sync_problems';
