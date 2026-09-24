-- 900 reaction_deliveries: a verdict for "decided to fire, could not"
-- (the reactions-update plan, RX — Phase B follow-up).
--
-- 880's `result` CHECK admits pending | fired | expired | suppressed_*. That
-- leaves no way to record the outcome an adversarial review of Phase B found
-- reachable in ORDINARY operation, not just after a crash:
--
--   * the reacting job was deleted or renamed between authoring and firing;
--   * RA-24 — the job consumes department-owned credentials that are unbound at
--     its scope, so a run would resolve nothing and be claimable by no
--     departmental runner (the scheduled path records a terminal `skipped` run
--     for exactly this, so it is visible rather than a log line);
--   * a transient failure inserting the pending run.
--
-- In all three the reactor decided to fire and could not. Before this migration
-- the row stayed at its CLAIM value, 'pending' — which is also what a crash
-- mid-decision leaves behind, so "we tried and failed" and "we never decided"
-- collapsed into one state. That is the precise ambiguity 880's own comment
-- says 'pending' exists to prevent, so leaving it would have made the delivery
-- log lie about the case it was designed for.
--
-- 'error' carries the reason in `detail`, keeping the invariant that every
-- decision writes a row that can say what it was.
--
-- reaction_deliveries has no inbound foreign keys and no triggers, and it is a
-- retention-pruned delivery log rather than a permanent record, so this rebuild
-- carries none of the hazards 890 had to stash around.
CREATE TABLE reaction_deliveries_new (
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
                     'error',                -- decided to fire, could not (detail says why)
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
INSERT INTO reaction_deliveries_new
    SELECT owner_source, owner_kind, owner_name, name, src_kind, src_run_id,
           outcome, result, detail, pending_run_id, delivered_at
      FROM reaction_deliveries;
DROP TABLE reaction_deliveries;
ALTER TABLE reaction_deliveries_new RENAME TO reaction_deliveries;

CREATE INDEX idx_reaction_deliveries_src ON reaction_deliveries(src_run_id);
CREATE INDEX idx_reaction_deliveries_fired
    ON reaction_deliveries(owner_source, owner_kind, owner_name, name, delivered_at)
    WHERE result = 'fired';
