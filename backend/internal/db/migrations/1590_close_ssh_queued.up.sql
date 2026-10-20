-- 1590_close_ssh_queued — a run still queued for the in-app SSH executor of
-- 2.2.x is closed as skipped, with the reason (2.5.0 C11, present defect 80;
-- the owner's answer to question 103).
--
-- Until 2.5.0 the 2.3.0 local-runner upgrade pass met these rows at the first
-- start of 2.3.0: it moved them to the runner executor when the SSH executor
-- had been on, and closed them as skipped when it had been off (a 2.2
-- installation with the executor off and `defaultExecutor` left at its default,
-- `ssh`, queued every shell fire there, and nothing ever claimed them). 2.5.0
-- retired that pass (C8.0), and a 2.2.3 installation that upgrades straight to
-- 2.5.0 kept them queued for good: nothing claims an `ssh` row since 2.3.0, the
-- retention sweep touches no queued row, and 1480 made each Global's, so they
-- counted against Global's waiting cap (at the default cap of 5, every Global
-- fire was skipped, which on 2.2.x data is every Git job) and held a Forbid or
-- Queue job's concurrency key, so that job never fired again.
--
-- The pass's own statement for the "off" case, for every such row. A row that
-- was RUNNING for the executor when 2.2.x stopped is swept at startup already
-- (sshexec's lost-run sweep). On a database that has none (every installation
-- since 2.3.0) this changes nothing.
UPDATE runs
   SET status = 'skipped',
       completed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
       queued_reason = 'Skipped at the upgrade to 2.5.0: this run was queued for the in-app SSH executor of 2.2.x, which no longer exists. Run the job again.'
 WHERE status = 'queued' AND executor = 'ssh';
