-- 1030_concurrency_key_uids — AF-4b stage R2-3: the concurrency gate's default
-- key becomes the job's uid (the rbac2 plan).
--
-- THIS IS A DATA MIGRATION WITH NO SCHEMA CHANGE, and it exists because the
-- gate is enforced by STRING EQUALITY, not by a join.
-- `uq_runs_active_concurrency` (migration 300) is a partial UNIQUE index over
-- runs.concurrency_key for queued/running rows, and CheckForbid compares the
-- key a new fire computed against the keys already in flight. So on the release
-- where six producers start composing that key differently, an in-flight run
-- holding 'git/backup' would NOT be seen by a new fire computing 'uid-abc123':
-- the two strings do not match, the gate reports clear, and a Forbid job whose
-- entire purpose is never to overlap runs twice. Exactly the RX-25 failure,
-- caused by an upgrade rather than by a divergent producer.
--
-- Rewriting the in-flight rows closes that window. It is deliberately narrow:
--
--   * ACTIVE runs only (queued/running). A finished run's key is history —
--     rewriting it would edit the record of what actually happened, and nothing
--     compares against terminal rows anyway.
--   * PENDING (parked) rows too: promotion re-judges Forbid against the live
--     runs using the key frozen on the row, so a stale key there produces the
--     same double-run one tick later.
--   * ONLY where the key equals the OLD DEFAULT composition,
--     COALESCE(source,'git') || '/' || name. A key that differs is
--     operator-authored (jobs.concurrency_key) — a deliberately shared
--     namespace, the way two jobs are made to hold one gate, and re-keying it
--     would silently un-share them. R2-Q2 keeps it free text forever.
--   * ONLY where the job still exists and has a uid to move to.
--
-- Note the join carries source: two jobs may already share a name across the
-- git/amadeus pools, and each in-flight run must land on its OWN job's uid.

UPDATE runs
   SET concurrency_key = (
        SELECT j.uid FROM jobs j
         WHERE j.name = runs.job_name
           AND j.source = COALESCE(runs.job_source, 'git')
           AND j.uid IS NOT NULL AND j.uid != '')
 WHERE status IN ('queued', 'running')
   AND concurrency_key IS NOT NULL
   AND concurrency_key = COALESCE(job_source, 'git') || '/' || job_name
   AND EXISTS (
        SELECT 1 FROM jobs j
         WHERE j.name = runs.job_name
           AND j.source = COALESCE(runs.job_source, 'git')
           AND j.uid IS NOT NULL AND j.uid != '');

UPDATE pending_runs
   SET concurrency_key = (
        SELECT j.uid FROM jobs j
         WHERE j.name = pending_runs.name
           AND j.source = COALESCE(pending_runs.source, 'git')
           AND j.uid IS NOT NULL AND j.uid != '')
 WHERE status = 'pending'
   AND kind = 'job'
   AND concurrency_key IS NOT NULL
   AND concurrency_key = COALESCE(source, 'git') || '/' || name
   AND EXISTS (
        SELECT 1 FROM jobs j
         WHERE j.name = pending_runs.name
           AND j.source = COALESCE(pending_runs.source, 'git')
           AND j.uid IS NOT NULL AND j.uid != '');
