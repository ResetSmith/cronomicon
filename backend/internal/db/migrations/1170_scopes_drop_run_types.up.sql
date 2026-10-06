-- 1170_scopes_drop_run_types — remove a scope's "supported run types" (the
-- scope-tags plan, ST-12).
--
-- supported_types (003) held the operator-declared set for cronomicon-source
-- scopes, with a "bash floor"; capability_types and capability_json (110) held
-- the git-source copy plus its provenance. The set was ADVISORY from the day it
-- shipped: no claim query, gate or dispatch path ever read it. Its only
-- consumers were the Scopes table, a mismatch warning in the Run dialog and the
-- composer (which blamed runner capacity, something the set never predicted),
-- and an inference heuristic that marked a scope PowerShell-capable when its
-- inventory text contained "win-srv". Scope tags (1160) are what operators
-- wanted from that column; this migration removes the rest.
--
-- capability_json is RENAMED rather than dropped, because three of its five
-- keys are not about capability at all: `owner` (the pragma/sidecar owner),
-- `sidecarPath`, and `errors` (line-numbered pragma parse errors). It becomes
-- git_meta_json and loses `types` and `origin`. The next sync rewrites the blob
-- in the new shape anyway; the json_remove keeps a not-yet-re-synced row tidy.
--
-- DROP COLUMN / RENAME COLUMN need SQLite 3.35+ / 3.25+ (the bundled library is
-- 3.53). None of the three columns is indexed, a PK, a FK target, in a CHECK or
-- named by a trigger or view, which is what DROP COLUMN requires.
ALTER TABLE scopes DROP COLUMN supported_types;
ALTER TABLE scopes DROP COLUMN capability_types;
ALTER TABLE scopes RENAME COLUMN capability_json TO git_meta_json;
UPDATE scopes
   SET git_meta_json = json_remove(git_meta_json, '$.types', '$.origin')
 WHERE git_meta_json IS NOT NULL AND json_valid(git_meta_json);
