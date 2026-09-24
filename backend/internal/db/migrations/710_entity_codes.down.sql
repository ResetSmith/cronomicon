-- Reverse 710 by dropping the registry and the run column.
--
-- LOSSY, and unusually so: rolling back destroys the code→entity mapping, and
-- re-running the up migration will NOT reproduce it. AUTOINCREMENT hands out
-- fresh values from a counter that this DROP resets, and the backfill assigns by
-- current table contents — so any entity created, renamed or deleted in between
-- shifts every subsequent code. Log folders written before the rollback would
-- then belong to the wrong entity, or to none.
--
-- That is acceptable only because the folders themselves survive: runs.entity_code
-- is dropped, so a rolled-back server reads every run's log from the flat
-- {logDir}/{traceID}.log path and simply stops finding the foldered ones. The
-- logs are not destroyed, only unreferenced, and the per-folder _meta.json
-- sidecar still records what each folder was — which is the reason that sidecar
-- is written at all.
--
-- If you roll back and intend to roll forward again, keep a copy of
-- entity_codes first.
ALTER TABLE runs DROP COLUMN entity_code;
DROP INDEX IF EXISTS idx_entity_codes_lookup;
DROP INDEX IF EXISTS idx_entity_codes_live;
DROP TABLE IF EXISTS entity_codes;
