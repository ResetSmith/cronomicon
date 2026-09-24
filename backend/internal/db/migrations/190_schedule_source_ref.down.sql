-- Reverse 190. mattn/go-sqlite3 supports ALTER TABLE DROP COLUMN (070/160 precedent);
-- idx_def_schedules_owner does not reference source_ref, so it is unaffected.
ALTER TABLE definition_schedules DROP COLUMN source_ref;
