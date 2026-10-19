-- 1580_assignments — a definition given to an agency by commit (2.5.0 C10,
-- IN-12; questions 87 to 93).
--
-- A global administrator assigns a job (and, from C10.2, a workflow) of one
-- agency's repository to another agency: ONE commit into the target's
-- repository carrying the definition and what it needs that the target does
-- not have. Each YAML file carries metadata.origin for whoever reads the
-- repository; this is the record the server reads, for every file written,
-- a plain script included (which has nowhere to carry a stamp). It is what
-- "an update is available" (C10.3) compares: the origin file's hash at the
-- commit it was copied from, and the hash of what was committed, which tells
-- whether the agency has changed its copy since.
--
-- A file the target already had by name (question 90) was not written and has
-- no row. The rows go with the target agency.
CREATE TABLE assignments (
    id             TEXT PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('job', 'workflow')),
    origin_agency  TEXT NOT NULL,
    origin_repo    TEXT NOT NULL,
    origin_uid     TEXT NOT NULL,
    origin_name    TEXT NOT NULL,
    origin_path    TEXT NOT NULL,
    origin_commit  TEXT NOT NULL,
    target_agency  TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
    target_repo    TEXT NOT NULL,
    target_path    TEXT NOT NULL,
    target_scope   TEXT,
    commit_sha     TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     TEXT NOT NULL
);
CREATE INDEX idx_assignments_origin ON assignments(origin_repo, origin_path);
CREATE INDEX idx_assignments_target ON assignments(target_repo);

CREATE TABLE assignment_files (
    assignment_id  TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE,
    target_path    TEXT NOT NULL,
    origin_path    TEXT NOT NULL,
    role           TEXT NOT NULL CHECK (role IN ('definition', 'script', 'schedule', 'job', 'workflow')),
    origin_hash    TEXT NOT NULL,
    committed_hash TEXT NOT NULL,
    PRIMARY KEY (assignment_id, target_path)
);
