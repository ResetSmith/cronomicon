-- CA-1 (the ssh-user plan) — per-run SSH identity, frozen at enqueue.
--
-- ssh_user / ssh_credential carry the run's effective "connect as" override:
-- the remote username and the ssh_credentials LABEL (CA-Q2 — names only, never
-- key bytes; resolution to material stays at the executor/agent boundary).
-- Nullable and additive: NULL means "no override" and preserves the exact
-- pre-CA per-host resolution, so no backfill. The F3 override envelope records
-- only what the operator typed; these columns hold the merged effective value
-- so scheduled/workflow runs can carry a job-spec identity later (Phase B)
-- without re-reading a mutable jobs row at dispatch.
ALTER TABLE runs ADD COLUMN ssh_user TEXT;
ALTER TABLE runs ADD COLUMN ssh_credential TEXT;
