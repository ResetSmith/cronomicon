-- 1340_git_sync_problems — what a sync found wrong with a repository's files,
-- as rows (2.4.0, GR-30; Phase R3).
--
-- A sync has always reported the files it could not use and the things it
-- warns of. The errors went into one text column of the sync's history row,
-- joined together; the warnings went to the server's log. The administrators
-- of the one repository could read both. An agency that keeps a repository of
-- its own can read neither.
--
-- One row per problem, per repository: the file (empty for a problem with the
-- repository as a whole), what kind of definition it holds, whether it is an
-- error (the file was not synced) or a warning, and what is wrong. The rows
-- are the repository's problems as of its LAST sync, replaced by each sync in
-- that sync's own transaction; a problem that is still there keeps the moment
-- it was first seen. `pass` is the sync that last saw the row, and is how a
-- sync tells its own rows from the ones it is replacing.
CREATE TABLE git_sync_problems (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    TEXT NOT NULL,
    path       TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL DEFAULT '',
    severity   TEXT NOT NULL CHECK (severity IN ('error', 'warning')),
    message    TEXT NOT NULL,
    first_seen TEXT NOT NULL,
    last_seen  TEXT NOT NULL,
    pass       TEXT NOT NULL,
    UNIQUE (repo_id, path, kind, severity, message)
);
CREATE INDEX idx_git_sync_problems_repo ON git_sync_problems(repo_id, severity);
