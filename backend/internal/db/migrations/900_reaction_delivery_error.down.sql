-- Reverse 900 — narrow the result CHECK back, dropping 'error'.
--
-- ⚠️ Fails while any row carries 'error', which is correct: silently relabelling
-- a failed delivery as 'pending' would restore the very ambiguity 900 removed.
-- Delete those rows first if a rollback is genuinely intended — they are
-- delivery-log entries, not runs, so nothing else references them.
CREATE TABLE reaction_deliveries_old (
    owner_source TEXT NOT NULL,
    owner_kind   TEXT NOT NULL,
    owner_name   TEXT NOT NULL,
    name         TEXT NOT NULL,
    src_kind     TEXT NOT NULL CHECK (src_kind IN ('job','workflow')),
    src_run_id   TEXT NOT NULL,
    outcome      TEXT NOT NULL,
    result       TEXT NOT NULL CHECK (result IN (
                     'pending',
                     'fired',
                     'expired',
                     'suppressed_disabled',
                     'suppressed_paused',
                     'suppressed_calendar',
                     'suppressed_depth',
                     'suppressed_rate'
                 )),
    detail       TEXT,
    pending_run_id TEXT,
    delivered_at TEXT NOT NULL,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name, src_kind, src_run_id)
);
INSERT INTO reaction_deliveries_old
    SELECT owner_source, owner_kind, owner_name, name, src_kind, src_run_id,
           outcome, result, detail, pending_run_id, delivered_at
      FROM reaction_deliveries;
DROP TABLE reaction_deliveries;
ALTER TABLE reaction_deliveries_old RENAME TO reaction_deliveries;

CREATE INDEX idx_reaction_deliveries_src ON reaction_deliveries(src_run_id);
CREATE INDEX idx_reaction_deliveries_fired
    ON reaction_deliveries(owner_source, owner_kind, owner_name, name, delivered_at)
    WHERE result = 'fired';
