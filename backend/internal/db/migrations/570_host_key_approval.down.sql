-- Reverse 570. Drop the scan-request column and the pending_host_keys table
-- (SQLite DROP COLUMN / DROP TABLE, precedented in prior down migrations).
-- Operator trust state; nothing else references it.
ALTER TABLE runners DROP COLUMN keyscan_requested;
DROP INDEX IF EXISTS idx_pending_host_keys_runner;
DROP INDEX IF EXISTS ux_pending_host_keys_target;
DROP TABLE pending_host_keys;
