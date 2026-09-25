-- AR (ad-hoc run scheduling) — deferred manual runs.
--
-- "Run this job at 5pm" from the Run dialog. A pending run is a REAL run's
-- frozen trigger, parked until its instant: for a job, params_json holds the
-- fully-validated scheduler.EnqueueParams (env snapshot, override envelope,
-- targeting, identity — everything the Run dialog collected); for a workflow it
-- is NULL and the promotion loop fires the engine directly. At run_at the
-- promotion loop turns the row into an ordinary `runs` row via the existing
-- enqueue seam and deletes it — History then owns the record.
--
-- Deliberately NOT a schedule entry: a one-shot schedule (v0.55.19) mutates the
-- job definition, is Admin-gated (requireCompose), and cannot carry the run
-- envelope. And deliberately NOT a `runs` row with a not-before: that would
-- thread a new predicate through both hot claim queries and count against the
-- concurrency cap from insert. A pending run does not exist for dispatch or
-- concurrency purposes until it is promoted — which is also WHEN the cap and
-- Forbid policy are correctly judged.
--
-- status: 'pending' rows are due (or waiting); 'missed' rows passed run_at by
-- more than the promotion grace (24h — a stale fire long after the chosen
-- instant is more surprising than a missed one) or were blocked past it, and
-- are kept visible rather than silently vanishing. Fired and cancelled rows are
-- DELETED — History and the Change Log carry those trails.
CREATE TABLE pending_runs (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('job','workflow')),
    name         TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','cronomicon')),
    scope        TEXT,                 -- job owner's effective scope, for the SU-2 read filter
    run_at       TEXT NOT NULL,        -- RFC3339 UTC instant to fire at
    scheduled_by TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','missed')),
    miss_reason  TEXT,
    params_json  TEXT                  -- frozen scheduler.EnqueueParams (job rows only)
);

CREATE INDEX idx_pending_runs_due ON pending_runs(status, run_at);

-- Deleting a definition takes its parked runs with it (same rule as the
-- definition_schedules cascade): a pending run for a job that no longer exists
-- could only fail at promotion.
CREATE TRIGGER trg_pending_runs_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM pending_runs WHERE kind = 'job' AND source = OLD.source AND name = OLD.name;
END;

CREATE TRIGGER trg_pending_runs_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM pending_runs WHERE kind = 'workflow' AND source = OLD.source AND name = OLD.name;
END;
