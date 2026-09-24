-- Reverse 080.
ALTER TABLE bastions  DROP COLUMN last_checked_at;
ALTER TABLE bastions  DROP COLUMN status;
ALTER TABLE ssh_hosts DROP COLUMN last_checked_at;
ALTER TABLE ssh_hosts DROP COLUMN status;
