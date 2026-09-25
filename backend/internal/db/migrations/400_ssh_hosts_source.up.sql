-- 400_ssh_hosts_source — host provenance for inventory import (Ansible-inventory
-- M4, plan §3.4). [Numbered 400, after the M1 390, so it always applies even on a
-- DB already migrated to 390 — golang-migrate never applies a lower version.]
--
-- Lets git-inventory hosts be imported INTO ssh_hosts so the in-app SSH executor
-- dials them with zero operator action, while keeping operator (cronomicon) rows
-- authoritative. `source` mirrors the git|cronomicon dual-source model (D4);
-- `scope_id`+`synced_at` let sync prune imported rows by owner without ever
-- touching operator rows. Existing rows default to 'cronomicon' (operator-authored).
--
-- A partial UNIQUE index (not a column constraint) enforces one git row per
-- (scope_id, hostname): SQLite can't ADD a UNIQUE column, and a full unique index
-- would fail on pre-existing duplicate operator hostnames (hostname is NOT unique).
ALTER TABLE ssh_hosts ADD COLUMN source TEXT NOT NULL DEFAULT 'cronomicon'
    CHECK (source IN ('git','cronomicon'));
ALTER TABLE ssh_hosts ADD COLUMN scope_id TEXT;   -- inventory scope an imported (git) row came from; NULL for operator rows
ALTER TABLE ssh_hosts ADD COLUMN synced_at TEXT;  -- stamped on import; NULL for cronomicon rows (prune key)
CREATE UNIQUE INDEX ux_ssh_hosts_inv ON ssh_hosts(scope_id, hostname) WHERE source='git';
