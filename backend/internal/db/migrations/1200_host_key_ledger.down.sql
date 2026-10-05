-- Reverse 1200 — drop the host-key ledger and the scan-target memory.
-- LOSSY by nature: the ledger exists nowhere else. Keys already on a runner
-- stay in its known_hosts file (nothing here ever removes one), and the
-- pending_host_keys rows that predate 1200 keep their approval stamps, so a
-- pre-1200 binary still delivers what was approved but not yet sent.
DROP INDEX IF EXISTS idx_host_key_ledger_to_untrust;
DROP INDEX IF EXISTS idx_host_key_ledger_to_trust;
DROP INDEX IF EXISTS idx_host_key_ledger_superseded;
DROP INDEX IF EXISTS idx_host_key_ledger_batch;
DROP INDEX IF EXISTS idx_host_key_ledger_target;
DROP TABLE IF EXISTS host_key_ledger;
DROP TABLE IF EXISTS host_key_scan_targets;
ALTER TABLE pending_host_keys DROP COLUMN host_name;
ALTER TABLE pending_host_keys DROP COLUMN scope_name;
ALTER TABLE pending_host_keys DROP COLUMN scope_id;
