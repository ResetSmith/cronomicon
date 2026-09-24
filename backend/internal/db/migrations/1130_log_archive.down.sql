-- Reverse 1130.
--
-- Not lossy: the archive markers describe objects that stay in the bucket, and
-- a subsequent up re-discovers them through the sweep's reconcile (SL-Q13,
-- "Sync now" with reconcile). The settings columns revert to the 060 shape; the
-- stored S3 connection fields are untouched. (DROP COLUMN has been used by this
-- migration set since 100; 1120 is the latest.)
DROP INDEX IF EXISTS idx_runs_log_pending;
ALTER TABLE runs DROP COLUMN log_archive_state;
ALTER TABLE runs DROP COLUMN log_archived_at;
ALTER TABLE log_storage_config DROP COLUMN last_sync_error;
ALTER TABLE log_storage_config DROP COLUMN last_sync_finished_at;
ALTER TABLE log_storage_config DROP COLUMN last_sync_started_at;
ALTER TABLE log_storage_config DROP COLUMN archived_bytes;
ALTER TABLE log_storage_config DROP COLUMN archived_count;
ALTER TABLE log_storage_config DROP COLUMN sync_at;
ALTER TABLE log_storage_config DROP COLUMN sync_interval_sec;
ALTER TABLE log_storage_config DROP COLUMN sync_mode;
ALTER TABLE log_storage_config DROP COLUMN s3_ca_pem;
ALTER TABLE log_storage_config DROP COLUMN s3_use_ssl;
