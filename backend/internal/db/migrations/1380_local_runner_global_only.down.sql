-- 1380_local_runner_global_only (down) — the agencies the local runner served
-- before 2.4.0 are put back on its list, from the record the up migration kept.

DELETE FROM runner_agencies
 WHERE agency_id = 'global'
   AND runner_id IN (SELECT id FROM runners WHERE kind = 'server')
   AND EXISTS (SELECT 1 FROM settings WHERE key = 'upgrade.2.4.0.localRunnerServed');

INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id)
SELECT r.id, j.value
  FROM runners r, settings s, json_each(s.value) j
 WHERE r.kind = 'server' AND s.key = 'upgrade.2.4.0.localRunnerServed'
   AND j.value IN (SELECT id FROM agencies);

DELETE FROM settings WHERE key = 'upgrade.2.4.0.localRunnerServed';

-- The notice listing the agencies the upgrade took off the local runner is
-- this migration's; the older code neither writes nor resolves it (added in
-- 2.4.2).
DELETE FROM notices WHERE kind = 'local_runner_served';
