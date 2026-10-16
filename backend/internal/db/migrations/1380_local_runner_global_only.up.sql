-- 1380_local_runner_global_only (2.4.0, §0a, IN-3) — the local runner is
-- Global's and serves Global only. Until now a global administrator gave it a
-- serve list of any agencies (MA-11), and the upgrade to 2.3.0 put agencies on
-- it whose shell jobs the server had been running.
--
-- The agencies it served are recorded first (the notice local_runner_served
-- lists each, until that agency has an agent of its own), then its list is
-- narrowed to Global. Their scopes' shell jobs wait for an agent of their own
-- agency from here.

INSERT OR REPLACE INTO settings (key, value, last_modified_by, last_modified_at)
SELECT 'upgrade.2.4.0.localRunnerServed', json_group_array(ra.agency_id), 'migration 1380', strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM runner_agencies ra JOIN runners r ON r.id = ra.runner_id
 WHERE r.kind = 'server' AND ra.agency_id <> 'global'
HAVING COUNT(*) > 0;

DELETE FROM runner_agencies
 WHERE agency_id <> 'global'
   AND runner_id IN (SELECT id FROM runners WHERE kind = 'server');

INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id)
SELECT id, 'global' FROM runners WHERE kind = 'server';
