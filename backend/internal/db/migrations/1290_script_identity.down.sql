-- 1290_script_identity (down) — a script is its name again.
--
-- Lossy by nature: with one repository nothing is lost, but if a second
-- repository has supplied a script of a name Global's also has, only one can
-- keep that name. Global's is kept; the others are dropped, with their
-- bindings.

DROP TRIGGER IF EXISTS jobs_script_uid_cleanup;
DROP TRIGGER IF EXISTS reference_bindings_script_cleanup;

-- Bindings of scripts that are about to be dropped go first, while the uid
-- still says whose they are.
DELETE FROM reference_bindings
 WHERE owner_kind = 'script'
   AND owner_uid IN (
       SELECT s.uid FROM scripts s
        WHERE s.repo_id != 'global'
          AND EXISTS (SELECT 1 FROM scripts g WHERE g.repo_id = 'global' AND g.name = s.name));
DELETE FROM scripts
 WHERE repo_id != 'global'
   AND EXISTS (SELECT 1 FROM scripts g WHERE g.repo_id = 'global' AND g.name = scripts.name);
-- Two repositories other than Global's with one name between them: keep one.
DELETE FROM reference_bindings
 WHERE owner_kind = 'script'
   AND owner_uid IN (SELECT s.uid FROM scripts s
                      WHERE s.rowid NOT IN (SELECT MIN(rowid) FROM scripts GROUP BY name));
DELETE FROM scripts WHERE rowid NOT IN (SELECT MIN(rowid) FROM scripts GROUP BY name);

-- A script's bindings are name-keyed again (uq_reference_bindings_script is
-- the index for rows with no uid).
UPDATE reference_bindings SET owner_uid = NULL WHERE owner_kind = 'script';

ALTER TABLE runs DROP COLUMN script_uid;
DROP INDEX IF EXISTS idx_jobs_script_uid;
ALTER TABLE jobs DROP COLUMN script_uid;

CREATE TABLE scripts_old (
    name         TEXT PRIMARY KEY,
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
    prompts_json TEXT NOT NULL DEFAULT '[]'
);
INSERT INTO scripts_old (name, description, run_type, command, script, script_path, executor,
                         content_hash, source_path, synced_at, warnings, variables, tags,
                         project_root, prompts_json)
    SELECT name, description, run_type, command, script, script_path, executor,
           content_hash, source_path, synced_at, warnings, variables, tags,
           project_root, prompts_json
    FROM scripts;
DROP TABLE scripts;
ALTER TABLE scripts_old RENAME TO scripts;

CREATE TRIGGER reference_bindings_script_cleanup
AFTER DELETE ON scripts
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'script' AND owner_name = OLD.name;
END;
