-- 1100_activity_runner_name — name the runner on activity rows
-- (the activity-actors plan, AA-1).
--
-- Activity had no runner column. Both identities an operator searches by --
-- the person and the machine -- were encoded in `actor`, and each writer chose
-- its own convention: an email for session-triggered events, `runner:<id>` for
-- agent-written ones, plus the fixed vocabulary `ssh-executor` / `system` /
-- `scheduler`. So a runner filter built on `actor` would have offered
-- `runner:0198f0a0-…` -- filterable, unreadable.
--
-- This is a SNAPSHOT column, deliberately, and not:
--   * a parse of `actor` at read time -- the value there is an id, and
--     resolving it needs a join that dies with the runner;
--   * an FK to runners(id) -- a runner can be deregistered (the RL-band
--     reaper's RunnerDeregisterAfter sweep), and ON DELETE SET NULL would
--     erase the answer at exactly the moment someone asks "what did the
--     runner we retired last month actually run?". Same reasoning as
--     `runs.agency` (450) and `runs.entity_code` (710): history keeps its own
--     copy of the catalog value it referred to.
-- `actor` keeps its `runner:<id>` meaning untouched -- it is what the audit
-- stream (auditlog's Event projection) and downstream log consumers read, and
-- renaming there would be a silent contract change (AA-Q6).
ALTER TABLE activity ADD COLUMN runner_name TEXT;

-- Backfill, in the 1010 tradition: stamp ONLY where the join is exact, leave
-- NULL otherwise. A migration that invented a plausible name would be worse
-- than one that admits it cannot know.
--
-- (a) Run rows. The run knows its runner (runs.runner_id, migration 030) and
--     the runner knows its name. Agent-written run rows carry `runner:<id>`;
--     the actor predicate keeps SSH-executor runs (which have no runner) out.
UPDATE activity SET runner_name = (
	SELECT rn.name FROM runs r
	  JOIN runners rn ON rn.id = r.runner_id
	 WHERE r.id = activity.trace_id)
 WHERE kind IN ('run-start', 'run-end')
   AND trace_id IS NOT NULL
   AND actor LIKE 'runner:%';

-- (b) Runner-lifecycle config rows already carried the DISPLAY name in
--     `target`, as `runner:<name>` -- drain, keyscan, resync request/declare
--     and the re-admit notice. Those recover even when the runner is long
--     gone, which (a) cannot do.
UPDATE activity SET runner_name = substr(target, length('runner:') + 1)
 WHERE kind = 'config'
   AND target LIKE 'runner:%'
   AND runner_name IS NULL;

-- Partial: only ~a few percent of activity rows name a runner, and the index
-- exists to serve `?runner=` (AA-2), which never asks for NULL.
CREATE INDEX IF NOT EXISTS idx_activity_runner_at
	ON activity(runner_name, at DESC) WHERE runner_name IS NOT NULL;

-- Not part of the runner feature: `?actor=` has been a supported filter since
-- the endpoint shipped and every "My triggers" click is a full table scan
-- today. The AA band is what finally sends that param from the UI in anger.
CREATE INDEX IF NOT EXISTS idx_activity_actor_at ON activity(actor, at DESC);
