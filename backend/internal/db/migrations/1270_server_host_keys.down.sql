-- LOSSY. The two columns come back holding the keys that were parked when they
-- were dropped. A key approved for the local runner since then stays in the
-- ledger, where the older server does not read it: such a host is captured
-- again on the next connection, as it was before 2.3.0.
ALTER TABLE ssh_hosts ADD COLUMN host_key TEXT;
ALTER TABLE bastions ADD COLUMN host_key TEXT;

UPDATE ssh_hosts SET host_key = (
    SELECT c.host_key FROM carried_server_host_keys c
     WHERE c.kind = 'host' AND c.record_id = ssh_hosts.id ORDER BY c.id DESC LIMIT 1)
 WHERE id IN (SELECT record_id FROM carried_server_host_keys WHERE kind = 'host');

UPDATE bastions SET host_key = (
    SELECT c.host_key FROM carried_server_host_keys c
     WHERE c.kind = 'bastion' AND c.record_id = bastions.id ORDER BY c.id DESC LIMIT 1)
 WHERE id IN (SELECT record_id FROM carried_server_host_keys WHERE kind = 'bastion');

DROP TABLE carried_server_host_keys;
