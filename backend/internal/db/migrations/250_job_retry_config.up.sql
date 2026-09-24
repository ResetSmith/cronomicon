-- 250 Job retry configuration (workflow-builder-update.md — WB-R1, D3).
--
-- `jobs.retries` already exists (005/170) but was never honored at run time. WB-R1
-- makes retry real for WORKFLOW steps: on a failed step the engine re-dispatches a
-- fresh child run up to `retries` times, waiting `backoff_seconds` between attempts,
-- then either continues (continue_on_error) or halts. Retry config resolves per
-- step as step-value ?? job-default, so these two columns are the job-level default.
--
-- backoff_seconds: seconds to wait between retry attempts (0 ⇒ immediate).
-- continue_on_error: when 1, a still-failing step is skipped-and-proceeded instead
-- of halting the workflow. Both additive (ALTER ADD COLUMN) — no rebuild. The
-- defaults reproduce today's behavior (no backoff, fail-fast).
ALTER TABLE jobs ADD COLUMN backoff_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN continue_on_error INTEGER NOT NULL DEFAULT 0;
