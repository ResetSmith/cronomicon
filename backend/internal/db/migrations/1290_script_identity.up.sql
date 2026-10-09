-- 1290_script_identity — a script has an identity of its own (2.4.0, GR-5).
--
-- Until now a script WAS its name: `scripts.name` was the primary key, a job
-- named its script in `jobs.script_ref`, a run remembered that name in
-- `runs.script_ref`, and a script's reference bindings were filed under
-- `owner_name`. That holds for as long as every script comes from one
-- repository. 2.4.0 gives each agency its own, and two repositories will both
-- have a `deploy.sh`.
--
-- This migration changes what a script IS and nothing about how many
-- repositories there are. There is still one, and every row is Global's:
--
--   * `scripts.uid` is the primary key. A script keeps its uid for as long as
--     its row lives: an edit in Git updates the row, a prune deletes it, and a
--     script that is removed and later added again is a NEW script.
--   * `scripts.repo_id` says which repository the row came from, and the name
--     is unique per repository, not per installation. The repositories table
--     itself arrives with the next migration of this release; until then the
--     one value is 'global' (repoid.Global), Global's repository, which is the
--     repository every installation has today.
--   * `jobs.script_uid` joins a job to its script; `script_ref` stays, as the
--     name the job's author wrote.
--   * `runs.script_uid` is the snapshot a run takes at enqueue, beside
--     `script_ref`. Dispatch finds a script's reference bindings from the RUN
--     (the manifest, the claim, the log redactor, the local runner), so the
--     run has to say which script it meant: with only the name it would mean
--     every repository's script of that name.
--   * a script's rows in `reference_bindings` carry `owner_uid`, as a job's
--     have since 1050, and the cleanup trigger keys on it.
--
-- The uid has a DEFAULT so that no statement can make a script without one
-- (sync mints its own, a UUIDv7 like every other id; the default covers a row
-- inserted by hand).

-- The trigger is declared ON scripts and names reference_bindings. It must not
-- exist while scripts is rebuilt (DROP TABLE would discard it silently, and an
-- ALTER … RENAME re-parses every trigger). It is recreated at the end, keyed
-- on the uid.
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;

CREATE TABLE scripts_new (
    uid          TEXT PRIMARY KEY NOT NULL DEFAULT (lower(hex(randomblob(16)))),
    repo_id      TEXT NOT NULL DEFAULT 'global',
    name         TEXT NOT NULL,
    description  TEXT,
    run_type     TEXT NOT NULL CHECK (run_type IN ('bash','ansible','terraform','powershell','perl','python')),
    command      TEXT,
    script       TEXT,
    script_path  TEXT,
    executor     TEXT CHECK (executor IN ('runner','ssh')),
    content_hash TEXT NOT NULL,
    source_path  TEXT,
    synced_at    TEXT NOT NULL,
    warnings     TEXT NOT NULL DEFAULT '[]',
    variables    TEXT NOT NULL DEFAULT '[]',
    tags         TEXT NOT NULL DEFAULT '[]',
    project_root TEXT,
    prompts_json TEXT NOT NULL DEFAULT '[]',
    UNIQUE (repo_id, name)
);
INSERT INTO scripts_new (uid, repo_id, name, description, run_type, command, script, script_path,
                         executor, content_hash, source_path, synced_at, warnings, variables, tags,
                         project_root, prompts_json)
    SELECT lower(hex(randomblob(16))), 'global', name, description, run_type, command, script, script_path,
           executor, content_hash, source_path, synced_at, warnings, variables, tags,
           project_root, prompts_json
    FROM scripts;
DROP TABLE scripts;
ALTER TABLE scripts_new RENAME TO scripts;

-- A job's script. Filled by name, which is exact today: one repository, one
-- script of a name. A job whose script_ref names no script (the script was
-- pruned and the job, being in-app, kept its copy of the body) gets NULL and
-- goes on running its copy, as it does now.
ALTER TABLE jobs ADD COLUMN script_uid TEXT;
UPDATE jobs
   SET script_uid = (SELECT s.uid FROM scripts s WHERE s.repo_id = 'global' AND s.name = jobs.script_ref)
 WHERE script_ref IS NOT NULL AND script_ref != '';
CREATE INDEX idx_jobs_script_uid ON jobs(script_uid) WHERE script_uid IS NOT NULL;

-- A run's script, for every run that named one: the queued and running ones
-- need it to find their bindings, and history keeps the same answer.
ALTER TABLE runs ADD COLUMN script_uid TEXT;
UPDATE runs
   SET script_uid = (SELECT s.uid FROM scripts s WHERE s.repo_id = 'global' AND s.name = runs.script_ref)
 WHERE script_ref IS NOT NULL AND script_ref != '';

-- A script's bindings move under its uid. A row whose script no longer exists
-- has no owner to move to: the cleanup trigger should have removed it when
-- the script went, and nothing could ever read it again, so it goes now.
UPDATE reference_bindings
   SET owner_uid = (SELECT s.uid FROM scripts s WHERE s.repo_id = 'global' AND s.name = reference_bindings.owner_name)
 WHERE owner_kind = 'script';
DELETE FROM reference_bindings WHERE owner_kind = 'script' AND owner_uid IS NULL;

-- ── triggers (created LAST, as in 1050) ─────────────────────────────────────

-- A script that goes takes its bindings with it, by uid: under
-- UNIQUE (repo_id, name) a name no longer says whose script it was, and a
-- prune of one repository's deploy.sh must not delete another's bindings.
CREATE TRIGGER reference_bindings_script_cleanup
AFTER DELETE ON scripts
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'script' AND owner_uid = OLD.uid;
END;

-- And it lets go of the jobs that used it. A Git job is rewritten by the same
-- sync that pruned the script; an in-app job keeps script_ref (the name its
-- author chose) and its copy of the body, and is joined again by name if a
-- script of that name returns (gitlab.upsertScripts).
CREATE TRIGGER jobs_script_uid_cleanup
AFTER DELETE ON scripts
BEGIN
    UPDATE jobs SET script_uid = NULL WHERE script_uid = OLD.uid;
END;
