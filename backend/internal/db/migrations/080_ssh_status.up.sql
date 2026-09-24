-- 080 SSH status: persist the connection-verification outcome of a "Test
-- connection" probe (ssh-update.md TC.1). Replaces the never-persisted
-- reachable/unreachable status with a real verified | cred_error | conn_error |
-- unverified lifecycle. New rows are 'unverified' until a test runs; a test
-- never auto-invalidates on edit — only a probe changes the value.
ALTER TABLE ssh_hosts ADD COLUMN status          TEXT NOT NULL DEFAULT 'unverified';
ALTER TABLE ssh_hosts ADD COLUMN last_checked_at TEXT;

ALTER TABLE bastions  ADD COLUMN status          TEXT NOT NULL DEFAULT 'unverified';
ALTER TABLE bastions  ADD COLUMN last_checked_at TEXT;
