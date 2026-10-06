-- 1210_runner_known_hosts — what a runner ACTUALLY trusts (the scope-bound-
-- runners plan, SB-5; runner protocol 14).
--
-- The ledger (1200) records what was approved here. It cannot see a
-- known_hosts file that was seeded on the runner host by hand, or edited since,
-- and it cannot know whether an approved key really reached the file. A v14
-- agent reports the file's entries — never the keys themselves, only each
-- line's host patterns, key type and fingerprint — and this table holds the
-- latest report, replaced whole each time.
--
-- That report is what lets the runner's panel show the file's keys in their
-- own section, mark which of them were approved here and which were not, and
-- stamp host_key_ledger.confirmed_at when an approved fingerprint turns up.
--
--   hosts   the line's host pattern(s), comma-joined. '' for a HASHED entry
--           (`|1|salt|hash`): the name cannot be recovered, by design, so such
--           a line is matched to approvals by fingerprint alone.
--   marker  '' | 'revoked' | 'cert-authority' — shown, never dropped: a revoked
--           key and a CA line mean something very different from a plain key.
--
-- This is state, not a record, so it follows its runner: ON DELETE CASCADE.
CREATE TABLE runner_known_hosts (
    runner_id   TEXT NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    line_no     INTEGER NOT NULL,
    hosts       TEXT NOT NULL DEFAULT '',
    hashed      INTEGER NOT NULL DEFAULT 0,
    marker      TEXT NOT NULL DEFAULT '',
    key_type    TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    PRIMARY KEY (runner_id, line_no)
);

-- When the last report arrived, whether it was cut short by the size cap, and
-- whether an operator has asked for a fresh one (delivered once, on the next
-- poll, like a keyscan request).
ALTER TABLE runners ADD COLUMN known_hosts_reported_at TEXT;
ALTER TABLE runners ADD COLUMN known_hosts_truncated INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners ADD COLUMN known_hosts_requested INTEGER NOT NULL DEFAULT 0;
