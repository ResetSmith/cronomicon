-- Reverse 1030 — put in-flight gate keys back into the source/name form the
-- pre-R2-3 producers compose, for exactly the same reason the up-migration
-- moved them: a rollback that left uid-keyed rows in flight would leave the
-- downgraded producers unable to see them, and the gate would be open on
-- precisely the runs it is holding.
--
-- The same narrowness applies. Only active runs and parked rows, and only where
-- the key is currently the job's uid — an operator-authored key never matched
-- a uid and must not be touched here either.

UPDATE runs
   SET concurrency_key = COALESCE(job_source, 'git') || '/' || job_name
 WHERE status IN ('queued', 'running')
   AND concurrency_key IS NOT NULL
   AND concurrency_key IN (
        SELECT j.uid FROM jobs j
         WHERE j.name = runs.job_name
           AND j.source = COALESCE(runs.job_source, 'git')
           AND j.uid IS NOT NULL AND j.uid != '');

UPDATE pending_runs
   SET concurrency_key = COALESCE(source, 'git') || '/' || name
 WHERE status = 'pending'
   AND kind = 'job'
   AND concurrency_key IS NOT NULL
   AND concurrency_key IN (
        SELECT j.uid FROM jobs j
         WHERE j.name = pending_runs.name
           AND j.source = COALESCE(pending_runs.source, 'git')
           AND j.uid IS NOT NULL AND j.uid != '');
