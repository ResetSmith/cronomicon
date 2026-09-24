-- 1120 Runner placement history (DR-7, the dr-readiness plan).
--
-- A runner that loses its identity re-registers and comes back with a NEW id, so
-- its agency membership (`runner_agencies`, keyed on runner id) and its tags
-- (`runners.tags`, a column on the row that was just deleted) are gone. The
-- runner then reports ONLINE, advertises correct capabilities, shows zero load,
-- and claims no work at all -- agency dispatch is hard and disjoint, and an RT
-- runner-tag pin will not match it either. That silence is the failure this
-- table exists to end.
--
-- Both deletion paths capture here, not just the obvious one: the operator
-- handler (register.go HandleDeregisterRunner) AND the reaper's offline sweep
-- (reaper.go deregisterRunner, past AMADEUS_RUNNER_DEREGISTER_AFTER -- default
-- 14 days). For disaster recovery the reaper is the DOMINANT path: in an outage
-- runners go offline, get reaped, and return to a server with no record of them.
-- Capturing only on manual deregister would miss the exact scenario this serves.
--
-- Snapshot, NOT a foreign key -- the referenced runners row is deleted, which is
-- the whole point. Same reasoning as `activity.runner_name` (1100) and
-- `runs.agency` (450): a snapshot survives the catalog churn that would
-- otherwise erase the answer at precisely the moment someone asks.
--
-- DR-Q2: this table is evidence for a SUGGESTION an operator confirms, never for
-- automatic re-binding. The runner `name` is self-declared by the agent at
-- registration, so healing placement by name alone would let any agent inherit
-- another runner's agency by claiming its name -- turning an enrollment
-- credential into a placement credential. `last_client_ip` is recorded because
-- it is the one signal the agent cannot freely assert (DR-Q7).

-- The observed client IP of the runner's most recent contact. Resolved through
-- httpx.ClientIP with the trusted-proxy allowlist -- NEVER r.RemoteAddr, which
-- behind a reverse proxy is the proxy for every runner and would make every
-- runner match every other one.
ALTER TABLE runners ADD COLUMN last_client_ip TEXT;

CREATE TABLE runner_placement_history (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The id that was deleted. Kept for the audit trail; runner ids are never
    -- reused, so this can never collide with a live row.
    runner_id         TEXT NOT NULL,
    name              TEXT NOT NULL,
    -- JSON arrays, snapshotted at deletion. agency_ids is the membership that
    -- `runner_agencies` held; tags/capabilities mirror the runners columns.
    agency_ids        TEXT NOT NULL DEFAULT '[]',
    tags              TEXT NOT NULL DEFAULT '[]',
    capabilities      TEXT NOT NULL DEFAULT '[]',
    last_client_ip    TEXT,
    deregistered_at   TEXT NOT NULL,
    -- Who did it: a session identity for the operator path, 'system' for the
    -- reaper. Mirrors AA-4's rule that a lifecycle event names its actor.
    deregistered_by   TEXT NOT NULL,
    deregistered_via  TEXT NOT NULL CHECK (deregistered_via IN ('operator','reaper')),
    -- DRF-3 / DRF-Q4: an operator's "this placement is not to be restored".
    -- Set on the SNAPSHOT, so a dismissed placement is never offered again to
    -- any runner -- the statement is about the placement, not a runner id. The
    -- row is kept (it is history) and still prunes on the retention window.
    dismissed_at      TEXT,
    dismissed_by      TEXT
);

-- The suggestion looks up by name (then weighs last_client_ip), and the
-- retention sweep prunes by age.
CREATE INDEX idx_runner_placement_history_name ON runner_placement_history(name);
CREATE INDEX idx_runner_placement_history_at ON runner_placement_history(deregistered_at);
