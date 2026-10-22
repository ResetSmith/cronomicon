-- 1650_run_notes (2.5.2 OP3; OP-8 to OP-16).
--
-- What a shift knew about a run: notes on one run (a job run or a workflow
-- run), and its acknowledgement, in one append-only record. The run's state is
-- its latest 'acknowledged' or 'unacknowledged' entry; a 'note' is text. Each
-- entry keeps who wrote it and how they stood to the run's agency when they did
-- (actor_relation, auth.Identity.RelationTo: member, global_admin, outside).
-- Nobody edits or deletes an entry; they go with their run (retention prunes
-- runs and workflow_runs by created_at, and these cascade). The change log
-- keeps a copy of each, under its own longer retention.
--
-- ⚠️ A later migration that rebuilds runs or workflow_runs (the 890 pattern:
-- CREATE _new / INSERT SELECT / DROP / RENAME) must keep these foreign keys
-- pointing at the rebuilt table; TestRunNotesGoWithTheirRun covers the cascade.
CREATE TABLE run_notes (
    id              INTEGER PRIMARY KEY,
    run_id          TEXT REFERENCES runs(id) ON DELETE CASCADE,
    workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('note', 'acknowledged', 'unacknowledged')),
    body            TEXT NOT NULL DEFAULT '' CHECK (length(body) <= 1000),
    author          TEXT NOT NULL,
    actor_relation  TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    CHECK ((run_id IS NULL) <> (workflow_run_id IS NULL))
);
CREATE INDEX idx_run_notes_run ON run_notes(run_id, id) WHERE run_id IS NOT NULL;
CREATE INDEX idx_run_notes_workflow_run ON run_notes(workflow_run_id, id) WHERE workflow_run_id IS NOT NULL;
