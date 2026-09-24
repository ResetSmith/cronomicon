-- Reverse 960 completely — the ledger and the column.
--
-- Dropping the ledger means a file already ingested would fire again if the
-- migration were re-applied and the file were still sitting in the directory.
-- That is the honest direction to fail (a duplicate run is visible and
-- correctable; a silently skipped arrival is not), but it is worth knowing
-- before rolling back a busy install.
DROP INDEX IF EXISTS idx_file_watch_sightings_job;
DROP INDEX IF EXISTS uq_file_watch_sightings;
DROP TABLE IF EXISTS file_watch_sightings;

ALTER TABLE jobs DROP COLUMN watch_json;
