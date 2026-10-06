-- 1200_host_key_ledger — a permanent record of every host-key decision (the
-- scope-bound-runners plan, SB-5).
--
-- A runner refuses any host that is not in its known_hosts file. Keys get there
-- by an operator approving them: the runner scans a host from its own network
-- position, the key lands in pending_host_keys (570), and approval sends it to
-- the runner. Until now the only record of that decision was a change-log row
-- that named the host and the fingerprint but NOT the runner, and the pending
-- row itself, which a re-scan of the same host overwrites (570's INSERT OR
-- REPLACE). Nothing could answer "which keys does this runner trust, and who
-- said so".
--
-- host_key_ledger is that answer. It is APPEND-ONLY for decisions: a re-scan, a
-- changed key or a removal adds a row and never rewrites one. Only the delivery
-- and lifecycle stamps on a row move.
--
--   decision   approved | rejected | removed
--              `removed` is the audit row for an operator taking a key away;
--              the key it removes is the earlier `approved` row, whose
--              superseded_at it stamps.
--   source     scan     the runner scanned the host and an operator approved it
--              pasted   an operator supplied the known_hosts line directly
--              carried  copied, on review, from another runner's trusted keys
--   batch_id   one operator action. A 47-key approval is 47 rows and one batch,
--              so the change log can carry one summary row that expands.
--
-- The four stamps on an `approved` row:
--
--   delivered_at   the key was sent to the runner (trust-hosts)
--   confirmed_at   the runner later REPORTED that fingerprint in its file
--   superseded_at  the key is no longer in force: replaced by a newer approval
--                  for the same (runner, host, key type), or removed. NULL means
--                  IN FORCE. A rejected or removed row is born superseded.
--   untrusted_at   the removal was sent to the runner (untrust-hosts). Sent for
--                  every retired approval, delivered or not: delivered_at is a
--                  to-do marker that "send again" clears, so it cannot say
--                  whether the line ever reached the file
--
-- Retention prunes on superseded_at, so an in-force key — NULL — can never
-- match a cutoff and is kept for as long as it is trusted.
--
-- runner_id has NO foreign key, like scope_runners.runner_id (1180): the record
-- must outlive the runner it is about. runner_name is the name at decision time.
CREATE TABLE host_key_ledger (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id             TEXT NOT NULL,
    runner_id            TEXT NOT NULL,
    runner_name          TEXT NOT NULL,
    scope_id             TEXT,
    scope_name           TEXT,
    host                 TEXT NOT NULL,     -- the known_hosts host pattern(s), as the line carries them
    host_name            TEXT,              -- the scope's name for that host, when the decision was made for a scope
    key_type             TEXT NOT NULL,
    fingerprint          TEXT NOT NULL,     -- SHA256:…
    known_hosts_line     TEXT NOT NULL,
    decision             TEXT NOT NULL CHECK (decision IN ('approved', 'rejected', 'removed')),
    source               TEXT NOT NULL CHECK (source IN ('scan', 'pasted', 'carried')),
    matched_server_pin   INTEGER NOT NULL DEFAULT 0,  -- the key equalled the one the server itself pins for this host
    previous_fingerprint TEXT,              -- the in-force key this one replaced, when it replaced one
    actor                TEXT NOT NULL,
    decided_at           TEXT NOT NULL,
    delivered_at         TEXT,
    confirmed_at         TEXT,
    superseded_at        TEXT,
    untrusted_at         TEXT
);

-- "What does this runner trust for this host" (supersede on approve, classify a
-- scan) and "what is still to be sent to this runner" (every poll).
CREATE INDEX idx_host_key_ledger_target ON host_key_ledger (runner_id, host, key_type);
CREATE INDEX idx_host_key_ledger_batch ON host_key_ledger (batch_id);
CREATE INDEX idx_host_key_ledger_superseded ON host_key_ledger (superseded_at);

-- "Is there anything to send to this runner" is asked by every connected
-- runner's long-poll, twice a second. These two partial indexes hold only the
-- rows that are waiting — a key to deliver, a key to take away — so on an idle
-- runner the answer is an empty index lookup, however many keys it trusts.
CREATE INDEX idx_host_key_ledger_to_trust ON host_key_ledger (runner_id)
    WHERE decision = 'approved' AND delivered_at IS NULL AND superseded_at IS NULL;
CREATE INDEX idx_host_key_ledger_to_untrust ON host_key_ledger (runner_id)
    WHERE decision = 'approved' AND superseded_at IS NOT NULL AND untrusted_at IS NULL;

-- What a scan was FOR. A scope scan asks the runner to dial addresses, and the
-- runner answers with keys for addresses; this is where the server remembers
-- which scope and which of its host names each address stood for, so the key
-- can be shown — and later recorded — against the host the operator knows.
-- One row per (runner, target); consumed when the key for that target arrives.
CREATE TABLE host_key_scan_targets (
    runner_id    TEXT NOT NULL,
    target       TEXT NOT NULL,
    scope_id     TEXT,
    scope_name   TEXT,
    host_name    TEXT,
    requested_by TEXT,
    requested_at TEXT NOT NULL,
    PRIMARY KEY (runner_id, target)
);

ALTER TABLE pending_host_keys ADD COLUMN scope_id TEXT;
ALTER TABLE pending_host_keys ADD COLUMN scope_name TEXT;
ALTER TABLE pending_host_keys ADD COLUMN host_name TEXT;

-- Carry over the decisions that are still on record. Earlier decisions for a
-- host that was re-scanned are gone — overwritten, which is the gap this table
-- closes — so this is every decision the database still knows of, not every
-- decision ever made. Each gets a batch of its own.
--
-- The ledger keys a decision on the host AS THE known_hosts LINE NAMES IT (its
-- first field: `host`, or `[host]:port`), which is what the runner looks up.
-- pending_host_keys.host held what the operator typed (`host:port`), so the
-- host is taken from the line, falling back to the typed form for a line with
-- no space in it — one that never was a known_hosts line.
INSERT INTO host_key_ledger
    (batch_id, runner_id, runner_name, host, key_type, fingerprint, known_hosts_line,
     decision, source, actor, decided_at, delivered_at, superseded_at)
SELECT 'migration:1200:' || p.id, p.runner_id, COALESCE(rn.name, p.runner_id),
       CASE WHEN instr(p.known_hosts_line, ' ') > 1 THEN substr(p.known_hosts_line, 1, instr(p.known_hosts_line, ' ') - 1) ELSE p.host END,
       p.key_type, p.fingerprint, p.known_hosts_line,
       'approved', 'scan', COALESCE(p.approved_by, ''), p.approved_at, p.trusted_at, NULL
  FROM pending_host_keys p LEFT JOIN runners rn ON rn.id = p.runner_id
 WHERE p.approved_at IS NOT NULL;

-- The typed forms `web01` and `web01:22` were two pending rows and are one
-- known_hosts host, so the backfill above can leave two approvals in force for
-- one (runner, host, key type) — which the ledger never allows. Keep the newest;
-- the older ones are superseded as if the newer approval had replaced them, and
-- the runner is told to drop any whose line differs.
UPDATE host_key_ledger
   SET superseded_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
 WHERE decision = 'approved' AND superseded_at IS NULL
   AND EXISTS (SELECT 1 FROM host_key_ledger n
                WHERE n.runner_id = host_key_ledger.runner_id
                  AND n.host = host_key_ledger.host AND n.key_type = host_key_ledger.key_type
                  AND n.decision = 'approved' AND n.superseded_at IS NULL
                  AND (n.decided_at > host_key_ledger.decided_at
                       OR (n.decided_at = host_key_ledger.decided_at AND n.id > host_key_ledger.id)));

INSERT INTO host_key_ledger
    (batch_id, runner_id, runner_name, host, key_type, fingerprint, known_hosts_line,
     decision, source, actor, decided_at, superseded_at)
SELECT 'migration:1200:' || p.id, p.runner_id, COALESCE(rn.name, p.runner_id),
       CASE WHEN instr(p.known_hosts_line, ' ') > 1 THEN substr(p.known_hosts_line, 1, instr(p.known_hosts_line, ' ') - 1) ELSE p.host END,
       p.key_type, p.fingerprint, p.known_hosts_line,
       'rejected', 'scan', COALESCE(p.rejected_by, ''), p.rejected_at, p.rejected_at
  FROM pending_host_keys p LEFT JOIN runners rn ON rn.id = p.runner_id
 WHERE p.rejected_at IS NOT NULL AND p.approved_at IS NULL;
