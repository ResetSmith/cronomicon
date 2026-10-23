-- 1270 — the server's own host keys move to the reviewed ledger (2.3.0, Phase C).
--
-- Until now the server kept a key on each host record and each bastion
-- (ssh_hosts.host_key, bastions.host_key), CAPTURED on the first connection.
-- From 2.3.0 the server runs jobs as the local runner, and the local runner
-- trusts a host the way every runner does: by a key an operator approved,
-- recorded in host_key_ledger. The two columns go.
--
-- The keys in them are not thrown away: a host the server was connecting to
-- must go on working. But a ledger row needs a fingerprint, a known_hosts line
-- and the local runner's id, none of which SQL can produce (the row is created
-- at boot). So the keys are parked here, and the server turns each into an
-- approved ledger row (source 'carried', actor the upgrade) the first time it
-- starts: runner.CarryServerHostKeys. A row stays, stamped, so the rollback
-- has something to put back.
CREATE TABLE carried_server_host_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL CHECK (kind IN ('host', 'bastion')),
    record_id    TEXT NOT NULL,
    name         TEXT NOT NULL,      -- the host's hostname; the bastion's name
    hostname     TEXT,               -- the bastion's hostname (hosts may name it by either)
    address      TEXT,
    port         INTEGER,
    scope_id     TEXT,               -- the scope an imported host record belongs to
    owner_agency TEXT NOT NULL DEFAULT 'global',
    host_key     TEXT NOT NULL,      -- authorized-key form, as it was stored
    carried_at   TEXT,               -- when the server recorded it in the ledger
    note         TEXT,               -- why it was not carried, when it was not
    pattern      TEXT,               -- the known_hosts host it was (or would have been) recorded under
    fingerprint  TEXT                -- SHA256:… of the key, computed by the server
);

INSERT INTO carried_server_host_keys (kind, record_id, name, address, port, scope_id, owner_agency, host_key)
SELECT 'host', id, hostname, address, port, scope_id, owner_agency, host_key
  FROM ssh_hosts WHERE TRIM(COALESCE(host_key, '')) <> '';

INSERT INTO carried_server_host_keys (kind, record_id, name, hostname, address, port, owner_agency, host_key)
SELECT 'bastion', id, name, hostname, address, port, owner_agency, host_key
  FROM bastions WHERE TRIM(COALESCE(host_key, '')) <> '';

ALTER TABLE ssh_hosts DROP COLUMN host_key;
ALTER TABLE bastions DROP COLUMN host_key;
