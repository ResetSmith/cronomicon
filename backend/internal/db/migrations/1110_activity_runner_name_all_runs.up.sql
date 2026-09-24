-- 1110_activity_runner_name_all_runs — widen 1100's backfill to EVERY run row
-- (the activity-actors plan, AA-2; closes the gap AA-1 recorded).
--
-- 1100 stamped run rows whose actor matched `runner:%`. That predicate was a
-- proxy for "a runner agent wrote this row", and it is not the same question as
-- "did this run execute on a runner". The rows it missed:
--
--   * A STOPPED run. The kill handler writes the run-end row with the operator's
--     email as the actor, so `runner:%` never matched -- yet the run really did
--     execute on a runner, and "everything runner X ran" silently excluded
--     exactly the runs somebody intervened in.
--   * Reaper and drain-timeout run-ends, whose actor is `system`. Those DO carry
--     runner_name going forward (AA-1 stamps them), but their historical rows
--     were skipped for the same reason.
--
-- The right predicate is the JOIN itself: runs.runner_id is set on claim and is
-- authoritative about where a run executed, whoever happened to write the
-- activity row. An SSH-executor run has no runner_id and still resolves to
-- nothing, so dropping the actor test loses no safety -- it was never the thing
-- doing the work.
--
-- Idempotent and additive: `runner_name IS NULL` means re-running stamps only
-- what is still unstamped, and a row whose runner was deregistered stays NULL,
-- exactly as in 1100.
UPDATE activity SET runner_name = (
	SELECT rn.name FROM runs r
	  JOIN runners rn ON rn.id = r.runner_id
	 WHERE r.id = activity.trace_id)
 WHERE kind IN ('run-start', 'run-end')
   AND trace_id IS NOT NULL
   AND runner_name IS NULL
   AND EXISTS (SELECT 1 FROM runs r2 WHERE r2.id = activity.trace_id
                 AND r2.runner_id IS NOT NULL);
