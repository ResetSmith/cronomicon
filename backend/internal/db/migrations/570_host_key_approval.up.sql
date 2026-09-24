-- 570 Host-key scan & approve (runner-install-update-2.md Phase 5, D4: 4A).
--
-- Human-approved TOFU for target host keys. An SSH run that fails host
-- verification surfaces a host_key_unverified reason; the operator triggers a
-- scan (runners.keyscan_requested — a one-shot JSON host list delivered on the
-- next poll, mirroring resync_requested but carrying the hosts to scan); the
-- agent scans from its own vantage and uploads what it saw into
-- pending_host_keys; the operator approves a row (comparing the full SHA256
-- fingerprint out-of-band); the next poll delivers the approved known_hosts
-- line as a trust-hosts op and the agent appends it. Every scan/approve/reject
-- is auditable.
CREATE TABLE pending_host_keys (
    id               TEXT PRIMARY KEY,
    runner_id        TEXT NOT NULL,
    host             TEXT NOT NULL,     -- the target as the operator/run named it
    key_type         TEXT NOT NULL,     -- e.g. ssh-ed25519, ecdsa-sha2-nistp256
    fingerprint      TEXT NOT NULL,     -- SHA256:… for out-of-band comparison
    known_hosts_line TEXT NOT NULL,     -- the exact line the agent appends on approval
    scanned_at       TEXT NOT NULL,
    approved_at      TEXT,
    approved_by      TEXT,
    rejected_at      TEXT,
    rejected_by      TEXT,
    trusted_at       TEXT               -- set when the trust-hosts op was delivered
);

-- One pending row per (runner, host, key_type): a re-scan REPLACEs it (resets
-- the approval state) rather than piling up duplicates.
CREATE UNIQUE INDEX ux_pending_host_keys_target ON pending_host_keys(runner_id, host, key_type);
CREATE INDEX idx_pending_host_keys_runner ON pending_host_keys(runner_id);

-- One-shot per-runner scan request: a JSON array of hosts to scan, delivered
-- and cleared on the next poll (protocol v5+). NULL = nothing pending.
ALTER TABLE runners ADD COLUMN keyscan_requested TEXT;
