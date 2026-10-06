-- Reverse 1210 — drop the reported known_hosts view. Nothing is lost that the
-- runner cannot report again: the table only mirrors a file that lives on the
-- runner host.
ALTER TABLE runners DROP COLUMN known_hosts_requested;
ALTER TABLE runners DROP COLUMN known_hosts_truncated;
ALTER TABLE runners DROP COLUMN known_hosts_reported_at;
DROP TABLE IF EXISTS runner_known_hosts;
